// Command objbox 是极简自建 S3 兼容对象存储的单二进制入口。
//
// 子命令：
//
//	objbox serve   -addr 127.0.0.1:18930 -data /var/lib/objbox
//	objbox account add <name> [-note "..."] [-readonly] [-bucket NAME] [-no-bucket] [-data DIR]
//	objbox account list [-show-secret] [-data DIR]
//	objbox account rotate <name> [-data DIR]
//	objbox account disable|enable <name> [-data DIR]
//	objbox account remove <name> [-data DIR]
//	objbox account quota <name> <bytes> [-data DIR]
//	objbox presign -account <name> -bucket <b> -key <k> [-method GET|PUT] [-expires 3600]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/YLing2024/objbox/internal/auth"
	"github.com/YLing2024/objbox/internal/backend"
	"github.com/YLing2024/objbox/internal/server"
	"github.com/YLing2024/objbox/internal/usage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	defaultAddr = "127.0.0.1:18930"

	// 连接信息占位，M0 不接真实域名。
	placeholderEndpoint = "https://s3.example.com"
	placeholderRegion   = "us-east-1"
)

// defaultDataDir 是未显式指定 -data 时使用的数据目录。
// 声明为变量以便测试覆盖，避免测试真的写入 /var/lib/objbox。
var defaultDataDir = "/var/lib/objbox"

const (
	// accountAddUsage 是 account add 的确切用法；账号名不合法时原样提示。
	accountAddUsage = "用法: objbox account add <name> [-note ...] [-readonly] [-bucket NAME] [-no-bucket] [-data DIR]\n"
	// nameRuleText 与 account.NameRe 对应。
	nameRuleText = "账号名须为小写字母或数字开头，后接小写字母/数字/连字符，长度 1-32（不得以 - 开头）"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测试的入口：参数与输出流显式传入，便于单测而无需 os.Exit。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usageText(stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "account":
		return runAccount(args[1:], stdout, stderr)
	case "presign":
		return runPresign(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usageText(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令: %s\n", args[0])
		usageText(stderr)
		return 2
	}
}

func usageText(w io.Writer) {
	fmt.Fprint(w, `objbox - 极简自建 S3 兼容对象存储

用法:
  objbox serve   -addr 127.0.0.1:18930 -data /var/lib/objbox
  objbox account add <name> [-note "..."] [-readonly] [-bucket NAME] [-no-bucket] [-data DIR]
  objbox account list [-show-secret] [-data DIR]
  objbox account rotate <name> [-data DIR]
  objbox account disable|enable <name> [-data DIR]
  objbox account remove <name> [-data DIR]
  objbox account quota <name> <bytes> [-data DIR]
  objbox presign -account <name> -bucket <b> -key <k> [-method GET|PUT] [-expires 3600]
                [-endpoint https://s3.example.com] [-region us-east-1] [-data DIR]
`)
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "监听地址")
	dataDir := fs.String("data", defaultDataDir, "数据目录")
	accessLog := fs.Bool("access-log", false, "打印访问日志（Authorization 已脱敏）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Printf("创建数据目录失败: %v", err)
		return 1
	}
	store, err := account.Load(*dataDir)
	if err != nil {
		log.Printf("加载账号表失败: %v", err)
		return 1
	}
	srv, err := server.New(store)
	if err != nil {
		log.Printf("初始化服务失败: %v", err)
		return 1
	}
	defer srv.Close()
	srv.AccessLog = *accessLog

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("objbox 监听 %s（数据目录 %s，账号数 %d）", *addr, *dataDir, len(store.List()))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("服务退出: %v", err)
		return 1
	}
	return 0
}

func runAccount(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usageText(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("account "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "数据目录")

	switch sub {
	case "add":
		note := fs.String("note", "", "备注")
		readonly := fs.Bool("readonly", false, "只读账号")
		bucket := fs.String("bucket", "", "默认桶名（缺省为账号名）")
		noBucket := fs.Bool("no-bucket", false, "不自动建桶（autoCreateBucket=false）")
		if len(rest) < 1 {
			fmt.Fprint(stderr, accountAddUsage)
			return 2
		}
		// 注意：flag 包只解析名字之后的旗标，因此 -data 必须写在名字之后。
		name := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		if !account.ValidName(name) {
			fmt.Fprintf(stderr, "账号名 %q 不合法：%s\n", name, nameRuleText)
			fmt.Fprint(stderr, accountAddUsage)
			return 2
		}
		bucketName := *bucket
		if bucketName == "" {
			bucketName = name
		}
		// 显式指定桶名，或默认要建桶时，桶名都必须合法；不放宽校验。
		if *bucket != "" || !*noBucket {
			if err := backend.ValidateBucket(bucketName); err != nil {
				fmt.Fprintf(stderr, "桶名 %q 不合法：%v\n", bucketName, err)
				fmt.Fprint(stderr, accountAddUsage)
				return 2
			}
		}
		warnDefaultDataDir(stderr, *dataDir, flagWasSet(fs, "data"))
		return accountAdd(stdout, stderr, *dataDir, name, *note, *readonly, bucketName, *noBucket)

	case "list":
		showSecret := fs.Bool("show-secret", false, "显示完整 SK")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		warnDefaultDataDir(stderr, *dataDir, flagWasSet(fs, "data"))
		return accountList(stdout, stderr, *dataDir, *showSecret)

	case "quota":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "用法: objbox account quota <name> <bytes> [-data DIR]")
			return 2
		}
		name, rawBytes := rest[0], rest[1]
		if err := fs.Parse(rest[2:]); err != nil {
			return 2
		}
		if !account.ValidName(name) {
			fmt.Fprintf(stderr, "账号名 %q 不合法：%s\n", name, nameRuleText)
			return 2
		}
		quota, err := strconv.ParseInt(rawBytes, 10, 64)
		if err != nil || quota < 0 {
			fmt.Fprintln(stderr, "bytes 必须是非负整数")
			return 2
		}
		warnDefaultDataDir(stderr, *dataDir, flagWasSet(fs, "data"))
		return accountQuota(stdout, stderr, *dataDir, name, quota)

	case "rotate", "disable", "enable", "remove":
		if len(rest) < 1 {
			fmt.Fprintf(stderr, "用法: objbox account %s <name> [-data DIR]\n", sub)
			return 2
		}
		name := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		if !account.ValidName(name) {
			fmt.Fprintf(stderr, "账号名 %q 不合法：%s\n", name, nameRuleText)
			return 2
		}
		warnDefaultDataDir(stderr, *dataDir, flagWasSet(fs, "data"))
		return accountMutate(stdout, stderr, *dataDir, sub, name)

	default:
		fmt.Fprintf(stderr, "未知 account 子命令: %s\n", sub)
		usageText(stderr)
		return 2
	}
}

// flagWasSet 报告指定旗标是否被显式设置（用于区分默认值与用户显式传入）。
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// warnDefaultDataDir 在未显式指定 -data 时提示正在使用默认数据目录，
// 避免用户误以为写到了自己期望的目录（M4 §5.3）。
func warnDefaultDataDir(w io.Writer, dataDir string, explicitlySet bool) {
	if explicitlySet {
		return
	}
	fmt.Fprintf(w, "提示: 正在使用默认数据目录 %s\n", dataDir)
}

func loadStore(stderr io.Writer, dataDir string) (*account.Store, int) {
	store, err := account.Load(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "加载账号表失败: %v\n", err)
		return nil, 1
	}
	return store, 0
}

func accountAdd(stdout, stderr io.Writer, dataDir, name, note string, readonly bool, bucket string, noBucket bool) int {
	store, code := loadStore(stderr, dataDir)
	if store == nil {
		return code
	}
	opts := account.AddOptions{Bucket: bucket}
	if noBucket {
		disabled := false
		opts.AutoCreateBucket = &disabled
	}
	a, err := store.AddAccount(name, note, readonly, opts)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// 建账号即建桶：autoCreateBucket=true 时预建默认桶；失败则回滚账号创建，
	// 保证「建完就能用」。桶已存在视为成功（幂等）。
	if a.AutoCreateBucket {
		if err := backend.EnsureBucketAt(a.Root, a.Bucket); err != nil {
			_ = store.Remove(name)
			fmt.Fprintf(stderr, "创建默认桶失败，已回滚账号创建: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "name:     %s\n", a.Name)
	fmt.Fprintf(stdout, "AK:       %s\n", a.AK)
	fmt.Fprintf(stdout, "SK:       %s   （仅本次显示，请立即妥善保存）\n", a.SK)
	fmt.Fprintf(stdout, "root:     %s\n", a.Root)
	fmt.Fprintf(stdout, "bucket:   %s\n", a.Bucket)
	fmt.Fprintf(stdout, "auto-create-bucket: %v\n", a.AutoCreateBucket)
	fmt.Fprintf(stdout, "data:     %s\n", store.DataDir())
	fmt.Fprintf(stdout, "readonly: %v\n", a.Readonly)
	if a.Note != "" {
		fmt.Fprintf(stdout, "note:     %s\n", a.Note)
	}
	fmt.Fprintln(stdout, "S3 连接信息（占位）:")
	fmt.Fprintf(stdout, "  Endpoint:  %s\n", placeholderEndpoint)
	fmt.Fprintf(stdout, "  Region:    %s\n", placeholderRegion)
	fmt.Fprintln(stdout, "  PathStyle: on")
	return 0
}

func accountList(stdout, stderr io.Writer, dataDir string, showSecret bool) int {
	store, code := loadStore(stderr, dataDir)
	if store == nil {
		return code
	}
	accounts := store.List()
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tAK\tSK\tBUCKET\tAUTO-CREATE\tROOT\tSTATUS\tREADONLY\tUSAGE(B)")
	for _, a := range accounts {
		sk := account.MaskSecret(a.SK)
		if showSecret {
			sk = a.SK
		}
		status := "enabled"
		if a.Disabled {
			status = "disabled"
		}
		usageStr := "-"
		if size, err := usage.DirSize(a.Root); err == nil {
			usageStr = fmt.Sprintf("%d", size)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\t%s\t%v\t%s\n",
			a.Name, a.AK, sk, a.Bucket, a.AutoCreateBucket, a.Root, status, a.Readonly, usageStr)
	}
	w.Flush()
	return 0
}

func accountMutate(stdout, stderr io.Writer, dataDir, sub, name string) int {
	store, code := loadStore(stderr, dataDir)
	if store == nil {
		return code
	}
	switch sub {
	case "rotate":
		sk, err := store.Rotate(name)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "账号 %s 的 SK 已轮换（旧 SK 立即失效）\n", name)
		fmt.Fprintf(stdout, "SK: %s   （仅本次显示，请立即妥善保存）\n", sk)
	case "disable":
		if err := store.SetDisabled(name, true); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "账号 %s 已停用\n", name)
	case "enable":
		if err := store.SetDisabled(name, false); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "账号 %s 已启用\n", name)
	case "remove":
		if err := store.Remove(name); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "账号 %s 已从账号表移除（其 root 数据目录保留）\n", name)
	}
	return 0
}

func accountQuota(stdout, stderr io.Writer, dataDir, name string, quotaBytes int64) int {
	store, code := loadStore(stderr, dataDir)
	if store == nil {
		return code
	}
	if err := store.SetQuota(name, quotaBytes); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if quotaBytes == 0 {
		fmt.Fprintf(stdout, "账号 %s 的配额已清除（不限）\n", name)
	} else {
		fmt.Fprintf(stdout, "账号 %s 的配额已设为 %d 字节\n", name, quotaBytes)
	}
	return 0
}

// runPresign 生成预签名 URL 并打印，便于手机/浏览器直接下载。
func runPresign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("presign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDir, "数据目录")
	name := fs.String("account", "", "账号名")
	bucket := fs.String("bucket", "", "桶名")
	key := fs.String("key", "", "对象 key")
	method := fs.String("method", "GET", "HTTP 方法：GET 或 PUT")
	expires := fs.Int64("expires", 3600, "有效期秒数（上限 604800）")
	endpoint := fs.String("endpoint", placeholderEndpoint, "服务端点（占位示例）")
	region := fs.String("region", placeholderRegion, "区域")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *bucket == "" || *key == "" {
		fmt.Fprintln(stderr, "用法: objbox presign -account <name> -bucket <b> -key <k> [-method GET|PUT] [-expires 3600]")
		return 2
	}
	m := strings.ToUpper(*method)
	if m != http.MethodGet && m != http.MethodPut {
		fmt.Fprintln(stderr, "method 只支持 GET 或 PUT")
		return 2
	}
	if *expires <= 0 || time.Duration(*expires)*time.Second > auth.MaxPresignExpires {
		fmt.Fprintln(stderr, "expires 必须在 1 到 604800 秒之间")
		return 2
	}

	store, code := loadStore(stderr, *dataDir)
	if store == nil {
		return code
	}
	warnDefaultDataDir(stderr, *dataDir, flagWasSet(fs, "data"))
	acct, ok := store.Find(*name)
	if !ok {
		fmt.Fprintf(stderr, "账号 %q 不存在\n", *name)
		return 1
	}

	signed, err := buildPresignedURL(*endpoint, *region, m, *bucket, *key, *expires, acct)
	if err != nil {
		fmt.Fprintf(stderr, "生成预签名 URL 失败: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, signed)
	return 0
}

// buildPresignedURL 复用与认证层同一套 v4 signer 生成 query 签名 URL。
func buildPresignedURL(endpoint, region, method, bucket, key string, expires int64, acct *account.Account) (string, error) {
	base := strings.TrimRight(endpoint, "/") + "/" + bucket + "/" + key
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("X-Amz-Expires", strconv.FormatInt(expires, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		return "", err
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	signed, _, err := signer.PresignHTTP(context.Background(), creds, req, auth.UnsignedPayload, auth.Service, region, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return signed, nil
}
