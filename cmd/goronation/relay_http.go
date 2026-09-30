package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 要求の中継 (ADR 0019): ホストが socketpair の片端を fd として送ってくる。init は、その fd から HTTP の要求を 1 つだけ読み、
// 決まった上流 (127.0.0.1:<ポート>) に、Authorization を付け直して流し、応答は解釈せずに返す。解釈は最小にする (init は PID 1)。
const (
	// relayMaxLine・relayMaxHead・relayMaxHeaders は、要求の先頭 (要求行 + ヘッダ) の、1 行・全体のバイト数・ヘッダの数の上限。
	relayMaxLine    = 16 << 10
	relayMaxHead    = 32 << 10
	relayMaxHeaders = 64
	// relayMaxTarget は、要求の target の長さの上限。
	relayMaxTarget = 4 << 10
	// relayMaxBody は、要求の本文の上限 (opencode の API の本文は、指示の文字列だけ)。
	relayMaxBody = 1 << 20

	// relayHeadTimeout は、要求の先頭を読み切るまで。relayBodyIdle は、本文の読みの 1 回ごとの、無通信の上限。
	relayBodyIdle = 10 * time.Second
	// relayWriteTimeout は、ホスト (fd の相手) への 1 回の書き込みの上限 (読まない相手で、goroutine が居座るのを避ける)。
	relayWriteTimeout = 30 * time.Second
	// relayUpstreamIdle は、上流が何も返さない時間の上限 (SSE は、心拍で超えない)。
	relayUpstreamIdle = 2 * time.Minute
	// relayDialTimeout は、上流への接続の上限。
	relayDialTimeout = 5 * time.Second

	// relayBasicUser は、opencode の Basic 認証のユーザー名 (ADR 0020)。
	relayBasicUser = "opencode"
	// relayStreamPath は、長く生きる SSE の要求の path (ほかの短い要求と、同時数の枠を分ける)。
	relayStreamPath = "/api/event"
)

// relayHeadTimeout は、要求の先頭を読み切るまでの上限 (テストで縮めるので var)。
var relayHeadTimeout = 10 * time.Second

// relayMethods は、通す HTTP メソッド (opencode の API が使うもの)。CONNECT・TRACE・OPTIONS などは拒否する。
var relayMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// relayReplaced は、init が付け直すので、要求から捨てるヘッダ (小文字): 大文字小文字・重複・空白に関わらず捨て、init が 1 つずつ付ける。
// relayAllow は、それ以外に通すヘッダ (小文字。ADR 0030)。Content-Length は init が付け直す。許可外のヘッダは、捨てずに拒否する (400):
// 送り手はホストだけで、値を組み立てるのもホストなので、許可外が来るのは、ホストの誤りか、攻撃である。許可リストのヘッダの重複も拒否する。
var (
	relayReplaced = map[string]bool{"authorization": true, "proxy-authorization": true, "host": true, "connection": true}
	relayAllow    = map[string]bool{"content-type": true, "accept": true, "cache-control": true, "last-event-id": true}
)

// relayReject は、要求を拒否するヘッダ (小文字)。Transfer-Encoding は本文の長さの解釈が分かれる (smuggling) ので通さない。
var relayReject = map[string]int{"transfer-encoding": 501, "upgrade": 400, "expect": 417}

// relayError は、要求を拒否する理由と、ホストへ返す status。
type relayError struct {
	status int
	msg    string
}

func (e *relayError) Error() string { return e.msg }

func reject(status int, format string, a ...any) *relayError {
	return &relayError{status: status, msg: fmt.Sprintf(format, a...)}
}

// relayRequest は、検査を通った要求: 上流に書く先頭 (Host・Authorization を付け直したもの) と、本文の長さ。
type relayRequest struct {
	head    []byte
	bodyLen int64
	method  string
	path    string // target の path 部分 (? の前)
}

// isStream は、長く生きる SSE の要求か。
func (r relayRequest) isStream() bool { return r.method == "GET" && r.path == relayStreamPath }

func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// visibleASCII は、s が、印字可能な ASCII (0x20〜0x7e) と HT だけから成るか。
func visibleASCII(s string, allowHT bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' && allowHT {
			continue
		}
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// plainPath は、path が、上流の正規化で /api/ の外を指しうる形 (. や .. の区間・エンコードされた . / \) を含まないか。
func plainPath(path string) bool {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// readHeadLine は、CRLF で終わる 1 行 (CRLF を除く) を読む。LF だけの行・上限を超える行は拒否する。
func readHeadLine(br *bufio.Reader, budget *int) (string, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", reject(431, "行が長すぎる")
		}
		return "", err
	}
	*budget -= len(line)
	if *budget < 0 {
		return "", reject(431, "要求の先頭が大きすぎる")
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", reject(400, "行が CRLF で終わらない")
	}
	return string(line[:len(line)-2]), nil
}

// parseRequest は、br から要求の先頭を読んで検査し、上流に書く先頭を組み立てる。host は "127.0.0.1:<ポート>"、auth は Authorization の値。
// 拒否は *relayError (status つき)。読みの失敗 (EOF・タイムアウト) は、そのままの error。
func parseRequest(br *bufio.Reader, host, auth string) (*relayRequest, error) {
	budget := relayMaxHead
	first, err := readHeadLine(br, &budget)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(first, " ")
	if len(parts) != 3 {
		return nil, reject(400, "要求行の形が不正")
	}
	method, target, version := parts[0], parts[1], parts[2]
	if !relayMethods[method] {
		return nil, reject(405, "メソッドを通さない: %.16q", method)
	}
	if version != "HTTP/1.1" {
		return nil, reject(505, "HTTP/1.1 だけ")
	}
	if len(target) > relayMaxTarget || !strings.HasPrefix(target, "/api/") || !visibleASCII(target, false) || strings.ContainsAny(target, "#\\") {
		return nil, reject(400, "target は /api/ で始まる origin-form だけ")
	}
	path, _, _ := strings.Cut(target, "?")
	if !plainPath(path) {
		return nil, reject(400, "path に ..・. の区間・エンコードされた . / \\ は使えない (/api/ の外へ出さない)")
	}

	var (
		headers [][2]string
		seen    = map[string]bool{}
		lines   int // 付け直して捨てるものを含む、ヘッダの行の数
		clen    = int64(-1)
	)
	for {
		line, err := readHeadLine(br, &budget)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if lines++; lines > relayMaxHeaders {
			return nil, reject(431, "ヘッダが多すぎる")
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, reject(400, "行の折り返し (obs-fold) は通さない")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			return nil, reject(400, "ヘッダの形が不正")
		}
		for i := 0; i < len(name); i++ {
			if !isTokenByte(name[i]) {
				return nil, reject(400, "ヘッダ名に使えない文字")
			}
		}
		if !visibleASCII(value, true) {
			return nil, reject(400, "ヘッダの値に使えない文字")
		}
		value = strings.Trim(value, " \t")
		lower := strings.ToLower(name)
		if status, bad := relayReject[lower]; bad {
			return nil, reject(status, "ヘッダ %s は通さない", lower)
		}
		if lower == "content-length" {
			if clen >= 0 {
				return nil, reject(400, "Content-Length が重複している")
			}
			n, err := parseContentLength(value)
			if err != nil {
				return nil, err
			}
			clen = n
			continue
		}
		if relayReplaced[lower] {
			continue
		}
		if !relayAllow[lower] {
			return nil, reject(400, "ヘッダ %.32q は許可リストに無い", lower)
		}
		if seen[lower] {
			return nil, reject(400, "ヘッダ %s が重複している", lower)
		}
		seen[lower] = true
		headers = append(headers, [2]string{name, value})
	}
	if clen < 0 {
		clen = 0
	}

	var b bytes.Buffer
	b.WriteString(method + " " + target + " HTTP/1.1\r\n")
	b.WriteString("Host: " + host + "\r\n")
	b.WriteString("Authorization: " + auth + "\r\n")
	b.WriteString("Connection: close\r\n")
	b.WriteString("Content-Length: " + strconv.FormatInt(clen, 10) + "\r\n")
	for _, h := range headers {
		b.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	b.WriteString("\r\n")
	return &relayRequest{head: b.Bytes(), bodyLen: clen, method: method, path: path}, nil
}

// parseContentLength は、Content-Length の値 (10 進数の数字だけ。符号・コンマ・空・上限超えは拒否) を返す。
func parseContentLength(v string) (int64, error) {
	if v == "" || len(v) > 10 {
		return 0, reject(400, "Content-Length が不正")
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, reject(400, "Content-Length が不正")
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, reject(400, "Content-Length が不正")
	}
	if n > relayMaxBody {
		return 0, reject(413, "本文が大きすぎる")
	}
	return n, nil
}

// basicAuth は、ユーザー名 relayBasicUser とパスワード token の Basic 認証の値。
func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(relayBasicUser+":"+token))
}

// requestRelay は、ホストから fd で受けた要求を、上流の opencode に流す。1 つの fd は 1 つの要求だけ (Connection: close の強制)。
type requestRelay struct {
	port  int           // 上流の 127.0.0.1 のポート (init が決めた値。上流の出力では決めない)
	token func() string // Authorization に使うパスワードの供給元 (init が作った、この起動だけのトークン)
	// alive は、上流 (子) が生きているか (pidfd。ADR 0029 の L1)。nil なら確かめない。接続したあと、先頭 (トークン) を書く前に確かめる。
	alive func() bool
	all   chan struct{} // 先頭の検査中を含む、同時に扱う fd の上限
	short chan struct{} // 短い要求の上限
	sse   chan struct{} // SSE の上限 (短い要求が、SSE に枠を取られて通らなくならないように分ける)
	wg    sync.WaitGroup
	quit  atomic.Bool // 立つと、新しい fd を受け付けず、上流への接続の前の要求も閉じる
}

// stop は、要求の受け付けを止める (上流が死んだあと、同じポートを別のプロセスが bind しても、トークンを送らない)。
func (r *requestRelay) stop() { r.quit.Store(true) }

// 同時数の上限。
const (
	relayMaxInflight = 64
	relayMaxShort    = 32
	relayMaxStreams  = 8
)

func newRequestRelay(port int, token func() string) *requestRelay {
	return &requestRelay{
		port: port, token: token,
		all: make(chan struct{}, relayMaxInflight), short: make(chan struct{}, relayMaxShort), sse: make(chan struct{}, relayMaxStreams),
	}
}

// accept は、client (受けた fd の接続) の処理を始める。上限に達していれば、何も読まずに閉じる (待たせない)。
func (r *requestRelay) accept(client net.Conn) {
	if r.quit.Load() {
		client.Close()
		return
	}
	select {
	case r.all <- struct{}{}:
	default:
		client.Close()
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { <-r.all }()
		defer func() { recover() }() // PID 1 は、パニックで落とさない。この要求の fd だけを閉じる
		r.handle(client)
	}()
}

// handle は、client の要求を 1 つ処理し、client を閉じる。
func (r *requestRelay) handle(client net.Conn) {
	defer client.Close()
	host := "127.0.0.1:" + strconv.Itoa(r.port)
	client.SetReadDeadline(time.Now().Add(relayHeadTimeout))
	br := bufio.NewReaderSize(client, relayMaxLine)
	req, err := parseRequest(br, host, basicAuth(r.token()))
	if err != nil {
		var re *relayError
		if errors.As(err, &re) {
			writeStatus(client, re.status)
		}
		return
	}
	slot := r.short
	if req.isStream() {
		slot = r.sse
	}
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	default:
		writeStatus(client, 503)
		return
	}

	if r.quit.Load() {
		writeStatus(client, 503)
		return
	}
	up, err := net.DialTimeout("tcp", host, relayDialTimeout)
	if err != nil {
		writeStatus(client, 502)
		return
	}
	defer up.Close()

	// ADR 0029 L1: 接続してから、子が生きていることを確かめて、初めてトークンを書く。死んだ子の代わりに同じポートを bind した別のプロセスへの
	// 接続は、子の死の後にしか成立しないので、ここで弾ける。生きているときに張れた接続は、子の listen に張られている。
	if r.alive != nil && !r.alive() {
		writeStatus(client, 502)
		return
	}

	up.SetWriteDeadline(time.Now().Add(relayWriteTimeout))
	if _, err := up.Write(req.head); err != nil {
		writeStatus(client, 502)
		return
	}
	if req.bodyLen > 0 {
		// 本文は、ちょうど bodyLen バイトだけ流す。そのあとにクライアントが送ったもの (pipelining) は、上流に届けない。
		if _, err := io.CopyN(up, &idleReader{c: client, r: br, idle: relayBodyIdle}, req.bodyLen); err != nil {
			writeStatus(client, 400)
			return
		}
	}
	client.SetReadDeadline(time.Time{})
	up.SetWriteDeadline(time.Time{})

	// クライアントが閉じた (ホストが要求を取り消した) ら、上流も閉じる。ホストは、要求のあとに、書き側だけを閉じない (全部閉じる)。
	go func() {
		io.Copy(io.Discard, br)
		up.Close()
	}()
	// 応答は解釈せずに返す (SSE のため、バッファしない)。遅い読み手・無言の上流で、居座らない。
	io.Copy(&deadlineWriter{c: client, d: relayWriteTimeout}, &deadlineReader{c: up, d: relayUpstreamIdle})
}

// writeStatus は、本文の無い応答を client に書く (読まない相手で居座らないよう、書き込みに期限を付ける)。
func writeStatus(c net.Conn, status int) {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, statusText(status))
}

func statusText(status int) string {
	switch status {
	case 400:
		return "Bad Request"
	case 405:
		return "Method Not Allowed"
	case 413:
		return "Payload Too Large"
	case 417:
		return "Expectation Failed"
	case 431:
		return "Request Header Fields Too Large"
	case 501:
		return "Not Implemented"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 505:
		return "HTTP Version Not Supported"
	}
	return "Error"
}

// idleReader は、r から読むたびに、c の読みの期限を idle 先に更新する (本文が、少しずつでも進めば続ける)。
type idleReader struct {
	c    net.Conn
	r    io.Reader
	idle time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.c.SetReadDeadline(time.Now().Add(i.idle))
	return i.r.Read(p)
}

// deadlineReader・deadlineWriter は、読み・書きのたびに、期限を d 先に更新する。
type deadlineReader struct {
	c net.Conn
	d time.Duration
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	r.c.SetReadDeadline(time.Now().Add(r.d))
	return r.c.Read(p)
}

type deadlineWriter struct {
	c net.Conn
	d time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.c.SetWriteDeadline(time.Now().Add(w.d))
	return w.c.Write(p)
}
