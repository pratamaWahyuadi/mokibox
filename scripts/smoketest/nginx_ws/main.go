// nginx_ws proves that the WebSocket location we ship in deploy/nginx/*.conf
// really lets a WebSocket handshake and frames through a real nginx.
//
// It extracts the `location = /api/chat/ws { ... }` block from each nginx
// config, points its upstream at a tiny local echo server, runs that block in
// a throw-away nginx container (host network, port 18081) and checks:
//
//  1. a WebSocket opened through nginx completes the upgrade and echoes frames
//  2. a control location with the SAME proxying but WITHOUT the Upgrade /
//     Connection headers is refused, so the test can actually fail
//
// It does not need Postgres, Zitadel or the api-gateway. It needs Docker and
// the nginx:1.27-alpine image the stack already uses.
//
// Usage (from the repo root):
//
//	go run ./scripts/smoketest/nginx_ws
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	echoAddr      = "127.0.0.1:18080"
	nginxAddr     = "127.0.0.1:18081"
	containerName = "mokibox-nginx-ws-test"
	nginxImage    = "nginx:1.27-alpine"
)

var confFiles = []string{"deploy/nginx/local.conf", "deploy/nginx/default.conf"}

func main() {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("docker is required for this test (it runs a throw-away nginx container)")
		os.Exit(2)
	}

	startEchoServer()

	failed := 0
	for _, f := range confFiles {
		fmt.Printf("\n== %s ==\n", f)
		if err := testConfig(f); err != nil {
			fmt.Printf("❌ %v\n", err)
			failed++
		}
	}

	fmt.Println()
	if failed == 0 {
		fmt.Println("🎉 NGINX WEBSOCKET PROXYING WORKS FOR ALL CONFIGS")
		return
	}
	fmt.Printf("💥 %d CONFIG(S) FAILED\n", failed)
	os.Exit(1)
}

// startEchoServer plays the api-gateway: it upgrades /api/chat/ws and echoes.
func startEchoServer() {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade already wrote the 4xx response
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	})
	ln, err := net.Listen("tcp", echoAddr)
	if err != nil {
		log.Fatalf("cannot listen on %s (is something else using it?): %v", echoAddr, err)
	}
	go func() { _ = http.Serve(ln, mux) }()
}

// extractBlock returns the `location = /api/chat/ws { ... }` block of a config.
func extractBlock(conf string) (string, error) {
	start := strings.Index(conf, "location = /api/chat/ws {")
	if start < 0 {
		return "", fmt.Errorf("no `location = /api/chat/ws {` block found")
	}
	depth := 0
	for i := start; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return conf[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced braces in the location block")
}

func testConfig(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w (run from the repo root)", path, err)
	}
	block, err := extractBlock(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	const upstream = "http://api-gateway:8080"
	if !strings.Contains(block, upstream) {
		return fmt.Errorf("%s: block no longer proxies to %s; update this test", path, upstream)
	}
	block = strings.ReplaceAll(block, upstream, "http://"+echoAddr)

	generated := fmt.Sprintf(`server {
    listen %s;

    %s

    # Control: identical proxying but WITHOUT the Upgrade/Connection headers.
    location = /control/no-upgrade-headers {
        proxy_pass http://%s/api/chat/ws;
        proxy_http_version 1.1;
    }
}
`, nginxAddr, block, echoAddr)

	dir, err := os.MkdirTemp("", "nginx-ws-test")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	confPath := filepath.Join(dir, "default.conf")
	if err := os.WriteFile(confPath, []byte(generated), 0o644); err != nil {
		return err
	}

	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	// ",z" relabels the temp file for SELinux hosts (Fedora); it is a no-op elsewhere.
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", containerName,
		"--network", "host",
		"-v", confPath+":/etc/nginx/conf.d/default.conf:ro,z",
		nginxImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run failed: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }()

	if err := waitForPort(nginxAddr, 10*time.Second); err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		return fmt.Errorf("nginx did not start listening on %s: %v\n%s", nginxAddr, err, logs)
	}

	// 1. Through nginx: upgrade + echo.
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+nginxAddr+"/api/chat/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("WebSocket handshake through nginx FAILED (http status %d): %v", status, err)
	}
	defer conn.Close()
	fmt.Println("✅ handshake through nginx: 101 Switching Protocols")

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping-through-nginx")); err != nil {
		return fmt.Errorf("write through nginx: %w", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil || string(msg) != "ping-through-nginx" {
		return fmt.Errorf("echo through nginx failed: msg=%q err=%v", msg, err)
	}
	fmt.Println("✅ frames flow both ways through nginx")

	// 2. Control must FAIL, otherwise this test proves nothing.
	if c2, _, err := websocket.DefaultDialer.Dial("ws://"+nginxAddr+"/control/no-upgrade-headers", nil); err == nil {
		_ = c2.Close()
		return fmt.Errorf("control location (no Upgrade headers) unexpectedly upgraded; the test is not discriminating")
	}
	fmt.Println("✅ control without Upgrade/Connection headers is refused (so the headers are what makes it work)")
	return nil
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}
