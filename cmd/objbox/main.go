// Command objbox 是极简自建 S3 兼容对象存储的单二进制入口。
//
// 子命令：
//
//	objbox serve   -addr 127.0.0.1:18930 -data /var/lib/objbox
//	objbox account add <name> [-note "..."] [-readonly]
//	objbox account list [-show-secret]
//	objbox account rotate <name>
//	objbox account disable|enable <name>
//	objbox account remove <name>
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/YLing2024/objbox/internal/server"
	"github.com/YLing2024/objbox/internal/usage"
)

const (
	defaultAddr    = "127.0.0.1:18930"
	defaultDataDir = "/var/lib/objbox"

	// 连接信息占位，M0 不接真实域名。
	placeholderEndpoint = "https://s3.example.com"
	placeholderRegion   = "us-east-1"
)

func main() {
	if len(os.Args) < 2 {
		usageText()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "account":
		os.Exit(runAccount(os.Args[2:]))
	case "-h", "--help", "help":
		usageText()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usageText()
		os.Exit(2)
	}
}

func usageText() {
	fmt.Fprint(os.Stderr, `objbox - 极简自建 S3 兼容对象存储

用法:
  objbox serve   -addr 127.0.0.1:18930 -data /var/lib/objbox
  objbox account add <name> [-note "..."] [-readonly] [-data DIR]
  objbox account list [-show-secret] [-data DIR]
  objbox account rotate <name> [-data DIR]
  objbox account disable|enable <name> [-data DIR]
  objbox account remove <name> [-data DIR]
`)
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "监听地址")
	dataDir := fs.String("data", defaultDataDir, "数据目录")
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

func runAccount(args []string) int {
	if len(args) < 1 {
		usageText()
		return 2
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("account "+sub, flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir, "数据目录")

	switch sub {
	case "add":
		note := fs.String("note", "", "备注")
		readonly := fs.Bool("readonly", false, "只读账号")
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "用法: objbox account add <name> [-note ...] [-readonly] [-data DIR]")
			return 2
		}
		name := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		return accountAdd(*dataDir, name, *note, *readonly)

	case "list":
		showSecret := fs.Bool("show-secret", false, "显示完整 SK")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		return accountList(*dataDir, *showSecret)

	case "rotate", "disable", "enable", "remove":
		if len(rest) < 1 {
			fmt.Fprintf(os.Stderr, "用法: objbox account %s <name> [-data DIR]\n", sub)
			return 2
		}
		name := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		return accountMutate(*dataDir, sub, name)

	default:
		fmt.Fprintf(os.Stderr, "未知 account 子命令: %s\n", sub)
		usageText()
		return 2
	}
}

func loadStore(dataDir string) (*account.Store, int) {
	store, err := account.Load(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载账号表失败: %v\n", err)
		return nil, 1
	}
	return store, 0
}

func accountAdd(dataDir, name, note string, readonly bool) int {
	store, code := loadStore(dataDir)
	if store == nil {
		return code
	}
	a, err := store.Add(name, note, readonly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("name:     %s\n", a.Name)
	fmt.Printf("AK:       %s\n", a.AK)
	fmt.Printf("SK:       %s   （仅本次显示，请立即妥善保存）\n", a.SK)
	fmt.Printf("root:     %s\n", a.Root)
	fmt.Printf("readonly: %v\n", a.Readonly)
	if a.Note != "" {
		fmt.Printf("note:     %s\n", a.Note)
	}
	fmt.Println("S3 连接信息（占位）:")
	fmt.Printf("  Endpoint:  %s\n", placeholderEndpoint)
	fmt.Printf("  Region:    %s\n", placeholderRegion)
	fmt.Println("  PathStyle: on")
	return 0
}

func accountList(dataDir string, showSecret bool) int {
	store, code := loadStore(dataDir)
	if store == nil {
		return code
	}
	accounts := store.List()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tAK\tSK\tROOT\tSTATUS\tREADONLY\tUSAGE(B)")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\t%s\n",
			a.Name, a.AK, sk, a.Root, status, a.Readonly, usageStr)
	}
	w.Flush()
	return 0
}

func accountMutate(dataDir, sub, name string) int {
	store, code := loadStore(dataDir)
	if store == nil {
		return code
	}
	switch sub {
	case "rotate":
		sk, err := store.Rotate(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Printf("账号 %s 的 SK 已轮换（旧 SK 立即失效）\n", name)
		fmt.Printf("SK: %s   （仅本次显示，请立即妥善保存）\n", sk)
	case "disable":
		if err := store.SetDisabled(name, true); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Printf("账号 %s 已停用\n", name)
	case "enable":
		if err := store.SetDisabled(name, false); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Printf("账号 %s 已启用\n", name)
	case "remove":
		if err := store.Remove(name); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Printf("账号 %s 已从账号表移除（其 root 数据目录保留）\n", name)
	}
	return 0
}
