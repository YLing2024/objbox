package server

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strconv"
)

// respWriter 包装协议处理器的 ResponseWriter，统一补齐 HTTP 语义：
//
//   - 每个响应都带 `Server: objbox` 与随机 `x-amz-request-id`（覆盖 gofakes3
//     默认写入的 `Server: AmazonS3`）；
//   - 状态码 >= 400 的响应先缓冲，再往 S3 XML `<Error>` 里补齐
//     `<RequestId>` / `<Resource>` 字段（gofakes3 自身 RequestId 为空）；
//   - 416 响应带 `Content-Range: bytes */<size>`，size 由 Range 预检提供。
//
// 成功响应不缓冲，直接流式转发（大对象 GET 不受影响）。
type respWriter struct {
	http.ResponseWriter
	reqID string
	path  string

	wrote     bool
	status    int
	buffering bool
	buf       bytes.Buffer

	rangeSize    int64
	hasRangeSize bool
}

func newRespWriter(w http.ResponseWriter, reqID, path string) *respWriter {
	return &respWriter{ResponseWriter: w, reqID: reqID, path: path}
}

func (w *respWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status

	h := w.Header()
	h.Set("Server", "objbox")
	if w.reqID != "" {
		h.Set("x-amz-request-id", w.reqID)
	}
	if status == http.StatusRequestedRangeNotSatisfiable && w.hasRangeSize {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(w.rangeSize, 10))
	}
	if status >= 400 {
		w.buffering = true
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/xml")
		}
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *respWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.buffering {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush 透传，保持流式响应的即时性。
func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish 在处理器返回后调用，把缓冲的错误体补齐后落盘。
func (w *respWriter) finish() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if !w.buffering {
		return
	}
	body := w.buf.Bytes()
	if bytes.Contains(body, []byte("<Error")) {
		body = injectErrorFields(body, w.reqID, w.path)
		w.Header().Set("Content-Type", "application/xml")
	}
	if len(body) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(body) > 0 {
		_, _ = w.ResponseWriter.Write(body)
	}
}

// injectErrorFields 往 S3 XML 错误体里补齐 Message / RequestId / Resource（缺失时）。
func injectErrorFields(body []byte, reqID, path string) []byte {
	if !bytes.Contains(body, []byte("<Message>")) {
		code := extractTag(body, "Code")
		msg := errorMessage(code)
		if msg != "" {
			body = bytes.Replace(body, []byte("</Code>"),
				[]byte("</Code><Message>"+xmlEscape(msg)+"</Message>"), 1)
		}
	}
	if reqID != "" && !bytes.Contains(body, []byte("<RequestId>")) {
		body = bytes.Replace(body, []byte("</Error>"),
			[]byte("<RequestId>"+xmlEscape(reqID)+"</RequestId></Error>"), 1)
	}
	if path != "" && !bytes.Contains(body, []byte("<Resource>")) {
		body = bytes.Replace(body, []byte("</Error>"),
			[]byte("<Resource>"+xmlEscape(path)+"</Resource></Error>"), 1)
	}
	return body
}

// extractTag 从 XML 体里取第一个 <tag>...</tag> 的文本（够用即可，不引 XML 解析器）。
func extractTag(body []byte, tag string) string {
	open := []byte("<" + tag + ">")
	closeTag := []byte("</" + tag + ">")
	i := bytes.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := bytes.Index(rest, closeTag)
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

// errorMessage 为 gofakes3 未填 Message 的错误码补上 S3 风格文案。
func errorMessage(code string) string {
	switch code {
	case "NoSuchKey":
		return "The specified key does not exist."
	case "NoSuchBucket":
		return "The specified bucket does not exist"
	case "NoSuchUpload":
		return "The specified multipart upload does not exist. The upload ID might be invalid, or the multipart upload might have been aborted or completed."
	case "InvalidPart":
		return "One or more of the specified parts could not be found."
	case "InvalidPartOrder":
		return "The list of parts was not in ascending order. Parts must be ordered by part number."
	case "":
		return ""
	default:
		return code
	}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
