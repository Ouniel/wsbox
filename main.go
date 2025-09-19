package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const helpText = `wsbox [command] [flags]

Commands:
  server    启动文件服务器
  client    连接到服务器进行文件操作
  help      显示帮助信息

Server Usage:
  wsbox server [flags]

Server Flags:
  -addr string    服务器监听地址 (默认 ":8080")
  -dir string     文件存储目录 (默认 ".")
  -token string   访问Token (留空自动生成)

Client Usage:
  wsbox client [flags] <command> [args...]

Client Flags:
  -s string    WebSocket服务器地址 (默认 "ws://127.0.0.1:8080/ws")

Client Commands:
  list [dir]              列出目录内容（树状结构）
  add <local> [remote]    上传文件到服务器
  get <remote> [local]    从服务器下载文件

Examples:
  wsbox server -addr :8080 -dir ./files -token mysecret
  wsbox client -s ws://token@server:8080/ws list
  wsbox client -s ws://token@server:8080/ws add file.txt uploads/file.txt
`

/* ---------- 日志辅助 ---------- */
func logEvent(ip, action, event string) {
	fmt.Printf("[%s][%s][%s][%s]\n", ip, action, time.Now().Format("2006-01-02 15:04:05"), event)
}

/* ---------- 服务端 ---------- */
type serverCmd struct {
	addr  string
	dir   string
	token string
}

func (s *serverCmd) run() {
	if s.token == "" {
		b := make([]byte, 16)
		rand.Read(b)
		s.token = hex.EncodeToString(b)
	}
	fmt.Println("=== wsbox ===")
	fmt.Printf("sandbox: %s\n", s.dir)
	fmt.Printf("fixed token: %s\n", s.token)

	localMux := http.NewServeMux()
	localMux.HandleFunc("/", s.localHandler)
	localLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(localLn, localMux)
	localURL := "http://" + localLn.Addr().String()
	log.Printf("local file server @ %s", localURL)

	gwMux := http.NewServeMux()
	gwMux.HandleFunc("/ws", s.gatewayHandler(localURL))
	log.Printf("gateway websocket @ %s", s.addr)
	log.Fatal(http.ListenAndServe(s.addr, gwMux))
}

/* ---------- 服务端：本地文件处理（带日志） ---------- */
func (s *serverCmd) localHandler(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	path := r.URL.Path

	switch r.Method {
	case "GET":
		if path == "/_list" {
			dir := r.URL.Query().Get("dir")
			if dir == "" {
				dir = "/"
			}

			// 安全路径验证
			real, err := securePath(dir, s.dir)
			if err != nil {
				logEvent(clientIP, "LIST", "invalid path: "+err.Error())
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			// 检查目录是否存在
			stat, err := os.Stat(real)
			if err != nil {
				if os.IsNotExist(err) {
					logEvent(clientIP, "LIST", "directory not found: "+dir)
					http.Error(w, "directory not found", http.StatusNotFound)
				} else {
					logEvent(clientIP, "LIST", "stat failed: "+err.Error())
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
				return
			}

			// 确保是目录
			if !stat.IsDir() {
				logEvent(clientIP, "LIST", "not a directory: "+dir)
				http.Error(w, "not a directory", http.StatusBadRequest)
				return
			}

			entries, err := os.ReadDir(real)
			if err != nil {
				logEvent(clientIP, "LIST", "read dir failed: "+err.Error())
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			var names []string
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			logEvent(clientIP, "LIST", fmt.Sprintf("dir=%s count=%d", dir, len(names)))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(names)
			return
		}

		// 下载
		real, err := securePath(path, s.dir)
		if err != nil {
			logEvent(clientIP, "DOWNLOAD", "invalid path: "+err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fi, err := os.Stat(real)
		if err != nil || fi.IsDir() {
			logEvent(clientIP, "DOWNLOAD", "file not found: "+path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logEvent(clientIP, "DOWNLOAD", "file: "+path)
		w.Header().Set("Content-Disposition", `attachment; filename=`+strconv.Quote(filepath.Base(real)))
		http.ServeFile(w, r, real)

	case "POST":
		real, err := securePath(path, s.dir)
		if err != nil {
			logEvent(clientIP, "UPLOAD", "invalid path: "+err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// 安全检查：验证目录创建的安全性
		if err := s.secureCreateDir(filepath.Dir(real), s.dir, clientIP); err != nil {
			logEvent(clientIP, "UPLOAD", "secure mkdir failed: "+err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		f, err := os.Create(real)
		if err != nil {
			logEvent(clientIP, "UPLOAD", "create file failed: "+err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, err := io.Copy(f, r.Body)
		f.Close()
		if err != nil {
			logEvent(clientIP, "UPLOAD", "write body failed: "+err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logEvent(clientIP, "UPLOAD", fmt.Sprintf("file=%s size=%d", path, n))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, "ok")

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

/* ---------- 服务端：网关 ---------- */
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func (s *serverCmd) gatewayHandler(local string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}

			// 处理文本消息（请求头）
			if msgType == websocket.TextMessage {
				parts := strings.SplitN(string(payload), " ", 3)
				if len(parts) < 2 {
					continue
				}
				method, path := parts[0], parts[1]
				var body io.Reader

				// 对于POST请求，需要等待下一个二进制消息作为请求体
				if method == "POST" {
					// 读取文件数据
					_, fileData, err := conn.ReadMessage()
					if err != nil {
						conn.WriteMessage(websocket.TextMessage, []byte("ERR "+err.Error()))
						continue
					}
					// 使用bytes.NewReader来保持二进制数据完整性
					body = bytes.NewReader(fileData)
				} else if len(parts) == 3 {
					body = strings.NewReader(parts[2])
				}

				req, err := http.NewRequest(method, local+path, body)
				if err != nil {
					conn.WriteMessage(websocket.TextMessage, []byte("ERR "+err.Error()))
					continue
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil || resp == nil {
					conn.WriteMessage(websocket.TextMessage, []byte("ERR "+err.Error()))
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				// 统一协议：分两次发——状态头 + 正文
				header := fmt.Sprintf("%d %d", resp.StatusCode, len(b))
				conn.WriteMessage(websocket.TextMessage, []byte(header))
				conn.WriteMessage(websocket.BinaryMessage, b)
			}
		}
	}
}

/* ---------- 客户端 ---------- */
type clientCmd struct {
	server string
}

func (c *clientCmd) run(args []string) {
	if len(args) < 1 {
		fmt.Print(helpText)
		os.Exit(1)
	}
	cmd := args[0]
	switch cmd {
	case "list":
		dir := "/"
		if len(args) > 1 {
			dir = args[1]
		}
		c.list(dir)
	case "add":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, "missing local-file\n")
			os.Exit(1)
		}
		local := args[1]
		remote := filepath.Base(local)
		if len(args) > 2 {
			remote = args[2]
		}
		c.add(local, remote)
	case "get":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, "missing remote-file\n")
			os.Exit(1)
		}
		remote := args[1]
		local := filepath.Base(remote)
		if len(args) > 2 {
			local = args[2]
		}
		c.get(remote, local)
	case "help":
		fmt.Print(helpText)
		return
	default:
		fmt.Print(helpText)
		os.Exit(1)
	}
}

func (c *clientCmd) dial() *websocket.Conn {
	h := http.Header{}
	u, _ := url.Parse(c.server)
	if u.User != nil {
		h.Set("Authorization", "Bearer "+u.User.Username())
		c.server = strings.Replace(c.server, u.User.String()+"@", "", 1)
	}
	conn, _, err := websocket.DefaultDialer.Dial(c.server, h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	return conn
}

// displayTree 以树状结构显示文件列表
func displayTree(names []string, dirName string) {
	if dirName == "/" {
		dirName = "root"
	}
	fmt.Printf("%s/\n", dirName)
	
	for i, name := range names {
		isLast := i == len(names)-1
		if isLast {
			fmt.Printf("└─ %s\n", name)
		} else {
			fmt.Printf("├─ %s\n", name)
		}
	}
}

func (c *clientCmd) list(dir string) {
	conn := c.dial()
	defer conn.Close()

	req := fmt.Sprintf("GET /_list?dir=%s", url.QueryEscape(dir))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(req)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	// 连续读两条消息
	_, headerMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	parts := strings.Fields(string(headerMsg))
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "bad header:", string(headerMsg))
		return
	}
	status, _ := strconv.Atoi(parts[0])
	if status >= 400 {
		_, bodyMsg, _ := conn.ReadMessage()
		fmt.Fprintln(os.Stderr, "remote error:", string(bodyMsg))
		return
	}
	_, bodyMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	var names []string
	if err := json.Unmarshal(bodyMsg, &names); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	
	// 使用树状结构显示
	displayTree(names, dir)
}

func (c *clientCmd) add(local, remote string) {
	f, err := os.Open(local)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	if fi.IsDir() {
		fmt.Fprintln(os.Stderr, "directory upload not implemented")
		return
	}

	conn := c.dial()
	defer conn.Close()

	// 确保远程路径以/开头
	if !strings.HasPrefix(remote, "/") {
		remote = "/" + remote
	}

	// 首先发送请求头
	req := fmt.Sprintf("POST %s", remote)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(req)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// 然后发送文件内容
	data, err := io.ReadAll(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read file error:", err)
		return
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		fmt.Fprintln(os.Stderr, "write file data error:", err)
		return
	}

	// 读取响应
	_, headerMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, "read header error:", err)
		return
	}

	parts := strings.Fields(string(headerMsg))
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "bad header:", string(headerMsg))
		return
	}

	status, _ := strconv.Atoi(parts[0])
	if status >= 400 {
		_, bodyMsg, _ := conn.ReadMessage()
		fmt.Fprintln(os.Stderr, "remote error:", string(bodyMsg))
		return
	}

	// 读取响应体（即使成功也需要读取，以清空连接）
	_, bodyMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, "read body error:", err)
		return
	}

	if status >= 200 && status < 300 {
		fmt.Println("upload done:", string(bodyMsg))
	} else {
		fmt.Fprintln(os.Stderr, "upload failed:", string(bodyMsg))
	}
}

func (c *clientCmd) get(remote, local string) {
	conn := c.dial()
	defer conn.Close()

	// 确保远程路径以/开头
	if !strings.HasPrefix(remote, "/") {
		remote = "/" + remote
	}

	req := fmt.Sprintf("GET %s", remote)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(req)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	_, headerMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	parts := strings.Fields(string(headerMsg))
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "bad header:", string(headerMsg))
		return
	}
	status, _ := strconv.Atoi(parts[0])
	if status >= 400 {
		_, bodyMsg, _ := conn.ReadMessage()
		fmt.Fprintln(os.Stderr, "remote error:", string(bodyMsg))
		return
	}
	_, bodyMsg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	f, err := os.Create(local)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(bodyMsg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("download done ->", local)
}

// secureCreateDir 安全地创建目录，包含额外的安全检查
func (s *serverCmd) secureCreateDir(dirPath, rootPath, clientIP string) error {
	absRoot, _ := filepath.Abs(rootPath)

	// 检查目录是否已存在
	if _, err := os.Stat(dirPath); err == nil {
		return nil // 目录已存在，无需创建
	}

	// 检查路径深度，防止创建过深的目录结构
	relPath, err := filepath.Rel(absRoot, dirPath)
	if err != nil {
		return errors.New("invalid directory path")
	}

	// 限制目录深度为最多5层
	pathParts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(pathParts) > 5 {
		return errors.New("directory depth too deep (max 5 levels)")
	}

	// 检查每个路径部分是否安全
	for _, part := range pathParts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		// 检查是否包含危险字符
		if strings.ContainsAny(part, "<>:\"|?*") {
			return errors.New("directory name contains illegal characters")
		}
		// 限制目录名长度
		if len(part) > 50 {
			return errors.New("directory name too long (max 50 characters)")
		}
	}

	// 逐级创建目录，确保每一级都在安全范围内
	currentPath := absRoot
	for _, part := range pathParts {
		if part == "" || part == "." {
			continue
		}
		currentPath = filepath.Join(currentPath, part)

		// 确保当前路径仍在根目录内
		if !strings.HasPrefix(currentPath, absRoot) {
			return errors.New("directory creation would escape sandbox")
		}

		// 创建目录
		if err := os.Mkdir(currentPath, 0755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	return nil
}

func securePath(raw string, root string) (string, error) {
	clean := filepath.Clean("/" + raw)
	if strings.Contains(clean, "..") {
		return "", errors.New("illegal path")
	}
	absRoot, _ := filepath.Abs(root)
	target := filepath.Join(absRoot, clean)
	if !strings.HasPrefix(target, absRoot) {
		return "", errors.New("path escape")
	}
	return target, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(helpText)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "server":
		fs := flag.NewFlagSet("server", flag.ExitOnError)
		addr := fs.String("addr", ":8080", "gateway listen address")
		dir := fs.String("dir", ".", "sandbox directory")
		token := fs.String("token", "", "fixed token (auto-generated if empty)")
		fs.Parse(os.Args[2:])
		(&serverCmd{addr: *addr, dir: *dir, token: *token}).run()

	case "client":
		fs := flag.NewFlagSet("client", flag.ExitOnError)
		s := fs.String("s", "ws://127.0.0.1:8080/ws", "websocket server")
		fs.Parse(os.Args[2:])
		(&clientCmd{server: *s}).run(fs.Args())

	case "help":
		fmt.Print(helpText)
		os.Exit(0)

	default:
		fmt.Print(helpText)
		os.Exit(1)
	}
}