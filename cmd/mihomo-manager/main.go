package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"mihomo-proxy/internal/manager"
	"mihomo-proxy/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	bootstrap := flag.Bool("bootstrap", false, "initialize last acknowledged kernel configuration and exit")
	health := flag.Bool("healthcheck", false, "check manager and kernel health and exit")
	flag.Parse()
	addr := env("CONTROL_ADDR", "0.0.0.0:3481")
	coreAddr := env("MIHOMO_CONTROL_ADDR", "127.0.0.1:9090")
	if *health {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		client := http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
		res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return errors.New("manager or kernel unhealthy")
		}
		return nil
	}
	key := os.Getenv("ADMIN_KEY")
	if len(key) < 12 {
		return errors.New("ADMIN_KEY 必须设置且至少 12 字符")
	}
	dir, err := filepath.Abs(env("DATA_DIR", "./data"))
	if err != nil {
		return err
	}
	secret, err := manager.ReadSecret(dir)
	if err != nil {
		return err
	}
	store, err := manager.OpenStore(filepath.Join(dir, "manager.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	// The image owns the kernel installation; native development resolves it via PATH.
	kernel := manager.NewRuntime("mihomo", dir, "http://"+coreAddr, secret)
	m := &manager.Manager{Store: store, Kernel: kernel, Dir: dir, CoreAddr: coreAddr, Secret: secret}
	for _, a := range []string{addr, coreAddr} {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return err
		}
		if a == coreAddr {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return errors.New("内核控制接口必须绑定回环地址")
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return err
		}
		m.ReservedPorts = append(m.ReservedPorts, n)
	}
	if m.ReservedPorts[0] == m.ReservedPorts[1] {
		return errors.New("管理页与内核控制端口不能相同")
	}
	if *bootstrap {
		return m.Bootstrap(context.Background())
	}
	if _, err = os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		return fmt.Errorf("请先运行 -bootstrap 初始化配置: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupDone := make(chan struct{})
	go func() {
		defer close(startupDone)
		for {
			if _, err := kernel.Version(ctx); err == nil {
				result := m.Apply(ctx)
				if !result.Applied {
					log.Printf("启动配置待应用: %s", result.Error)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	server := &http.Server{Addr: addr, Handler: manager.NewServer(m, key).Handler(web.Handler()), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { log.Printf("Mihomo Manager listening on %s", addr); done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		stop()
		<-startupDone
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	<-startupDone
	return err
}
