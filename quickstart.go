package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

func localPeer(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	// IPv6 link-local addresses may carry an interface scope.
	for i, c := range host {
		if c == '%' {
			host = host[:i]
			break
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func prepareQuickstart(dir string) (Config, string, error) {
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Config{}, "", err
		}
		dir = filepath.Join(base, "open-vault")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Config{}, "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Config{}, "", err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return Config{}, "", errors.New("quickstart directory must be a private directory (0700), not a symlink")
	}
	path := filepath.Join(dir, "config.json")
	if _, err := os.Lstat(path); err == nil {
		cfg, err := loadConfig(path)
		return cfg, path, err
	} else if !os.IsNotExist(err) {
		return Config{}, path, err
	}
	tokenPath := filepath.Join(dir, "api-token")
	if _, err := os.Lstat(tokenPath); os.IsNotExist(err) {
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return Config{}, path, err
		}
		if err = writeNewPrivateFile(tokenPath, []byte(hex.EncodeToString(random[:])+"\n")); err != nil && !os.IsExist(err) {
			return Config{}, path, err
		}
	}
	token, err := readSecret(tokenPath)
	if err != nil || len(token) < 32 {
		return Config{}, path, errors.New("existing quickstart token is invalid; it has not been replaced")
	}
	socket := "/run/open-vault-cbt/agent.sock"
	if runtime.GOOS != "linux" {
		socket = filepath.Join(dir, "agent.sock")
	}
	cfg := Config{Listen: "127.0.0.1:8080", TokenFile: tokenPath, CBTSocket: socket}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Config{}, path, err
	}
	if err = writeNewPrivateFile(path, append(data, '\n')); err != nil && !os.IsExist(err) {
		return Config{}, path, err
	}
	cfg, err = loadConfig(path)
	return cfg, path, err
}

func writeNewPrivateFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func applyListenOptions(cfg *Config, lan bool, port int) error {
	host, existingPort, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	if port < 0 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if port != 0 {
		existingPort = strconv.Itoa(port)
	}
	if lan {
		host = "0.0.0.0"
		cfg.AllowLAN = true
	}
	cfg.Listen = net.JoinHostPort(host, existingPort)
	return nil
}

// Bind before displaying any success URLs. Only interactive quickstart may try
// the next ten ports; an explicitly chosen port or service config stays fixed.
func listenConsole(cfg *Config, automatic bool) (net.Listener, error) {
	host, portText, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, errors.New("监听端口无效，请填写 1 到 65535")
	}
	var lastErr error
	for offset := 0; offset <= 10 && port+offset <= 65535; offset++ {
		address := net.JoinHostPort(host, strconv.Itoa(port+offset))
		listener, err := net.Listen("tcp", address)
		if err == nil {
			cfg.Listen = net.JoinHostPort(host, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
			return listener, nil
		}
		lastErr = err
		// Windows reports Winsock WSAEADDRINUSE (10048), rather than the
		// portable syscall errno used by Unix.
		occupied := errors.Is(err, syscall.EADDRINUSE) || (runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10048)))
		if !automatic || !occupied {
			break
		}
	}
	return nil, fmt.Errorf("无法启动监听 %s：%w；请用 -port 指定其他端口，或检查占用程序", cfg.Listen, lastErr)
}

func printStartup(cfg Config, path string, showToken bool, token string) {
	host, port, _ := net.SplitHostPort(cfg.Listen)
	fmt.Printf("\nOpen Vault %s · 开发预览\n配置文件：%s\n", version, path)
	environment := inspectEnvironment(cfg.CBTSocket)
	fmt.Printf("当前环境：%s / %s / %s %s / 内核 %s\n%s\n", environment.OS, environment.Architecture, environment.Distribution, environment.DistributionVersion, environment.Kernel, environment.Message)
	for _, issue := range environment.Issues {
		fmt.Println("环境检查：" + issue)
	}
	if !environment.AgentSocketPresent {
		fmt.Println("后台服务未连接：本命令只启动网页，不会自动安装 Agent 或内核驱动。可运行 bash doctor.sh 查看诊断。")
	}
	if cfg.AllowLAN && net.ParseIP(host) != nil && net.ParseIP(host).IsUnspecified() {
		fmt.Printf("本机访问：http://127.0.0.1:%s\n", port)
		addresses, _ := net.InterfaceAddrs()
		seen := map[string]bool{}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() && !seen[ip.String()] {
				fmt.Printf("局域网访问：http://%s\n", net.JoinHostPort(ip.String(), port))
				seen[ip.String()] = true
			}
		}
		fmt.Println("请在同一局域网的电脑或手机浏览器打开地址。仅允许本机及私有网络来源。")
	} else {
		fmt.Printf("访问地址：http://%s\n", cfg.Listen)
	}
	if showToken {
		fmt.Printf("登录口令：%s\n", token)
	} else {
		fmt.Printf("登录口令保存在：%s\n", cfg.TokenFile)
	}
	fmt.Println("此版本的 CBT 增量恢复尚未通过验收，仅供测试。按 Ctrl+C 停止。")
}
