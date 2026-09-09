package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Kernel interface {
	Validate(context.Context, string) error
	Reload(context.Context, string) error
	Verify(context.Context, []int, []int) error
	Version(context.Context) (string, error)
	Delay(context.Context, string) (int, error)
	Connections(context.Context) (CoreConnections, error)
	CloseConnection(context.Context, string) error
	CloseConnections(context.Context) error
	// StreamLogs returns the kernel log stream. The caller closes it.
	StreamLogs(context.Context, string) (io.ReadCloser, error)
	RefreshRuleProvider(context.Context, string) error
	SelectProxy(ctx context.Context, group, name string) error
}

type Runtime struct {
	Binary, DataDir, URL, Secret string
	Client                       *http.Client
	// stream has no client timeout: the log endpoint stays open indefinitely
	// and is bounded by its context instead.
	stream *http.Client
}

func NewRuntime(binary, dir, endpoint, secret string) *Runtime {
	return &Runtime{
		Binary: binary, DataDir: dir, URL: strings.TrimRight(endpoint, "/"), Secret: secret,
		Client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
		stream: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

func (r *Runtime) Validate(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, r.Binary, "-t", "-d", r.DataDir, "-f", path).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if len(text) > 3000 {
			text = text[len(text)-3000:]
		}
		return fmt.Errorf("内核配置校验失败: %s (%w)", text, err)
	}
	return nil
}

func (r *Runtime) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.Secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("内核返回 %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return nil
}

func (r *Runtime) Reload(ctx context.Context, path string) error {
	return r.request(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": path}, nil)
}
func (r *Runtime) Version(ctx context.Context) (string, error) {
	var result struct {
		Version string `json:"version"`
	}
	err := r.request(ctx, http.MethodGet, "/version", nil, &result)
	return result.Version, err
}
func (r *Runtime) Delay(ctx context.Context, id string) (int, error) {
	var result struct {
		Delay int `json:"delay"`
	}
	err := r.request(ctx, http.MethodGet, "/proxies/"+url.PathEscape("node-"+id)+"/delay?timeout=5000&url="+url.QueryEscape("https://www.gstatic.com/generate_204"), nil, &result)
	return result.Delay, err
}

func (r *Runtime) Connections(ctx context.Context) (CoreConnections, error) {
	var result CoreConnections
	err := r.request(ctx, http.MethodGet, "/connections", nil, &result)
	return result, err
}

func (r *Runtime) CloseConnection(ctx context.Context, id string) error {
	return r.request(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil, nil)
}

func (r *Runtime) CloseConnections(ctx context.Context) error {
	return r.request(ctx, http.MethodDelete, "/connections", nil, nil)
}

func (r *Runtime) RefreshRuleProvider(ctx context.Context, name string) error {
	return r.request(ctx, http.MethodPut, "/providers/rules/"+url.PathEscape(name), nil, nil)
}

// SelectProxy points a select group at one of its members. store-selected is
// off, so the choice lives only until the next reload.
func (r *Runtime) SelectProxy(ctx context.Context, group, name string) error {
	return r.request(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": name}, nil)
}

// StreamLogs opens the kernel log stream. Without a websocket upgrade the
// kernel emits one JSON object per line, which needs no extra dependency.
func (r *Runtime) StreamLogs(ctx context.Context, level string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL+"/logs?level="+url.QueryEscape(level), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Secret)
	res, err := r.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		res.Body.Close()
		return nil, fmt.Errorf("内核返回 %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return res.Body, nil
}

func socksProbe(ctx context.Context, port int) error {
	conn, err := (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err = conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err = io.ReadFull(conn, reply); err != nil {
		return err
	}
	if !bytes.Equal(reply, []byte{5, 0}) {
		return errors.New("端口未提供预期的 mixed 代理")
	}
	return nil
}

// PUT /configs alone does not prove that the kernel successfully bound its sockets.
func (r *Runtime) Verify(ctx context.Context, active, removed []int) error {
	deadline := time.Now().Add(4 * time.Second)
	for {
		var failure error
		for _, port := range active {
			if err := socksProbe(ctx, port); err != nil {
				failure = fmt.Errorf("监听 %d 未就绪: %w", port, err)
				break
			}
		}
		if failure == nil {
			for _, port := range removed {
				conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err == nil {
					conn.Close()
					failure = fmt.Errorf("旧监听 %d 尚未关闭", port)
					break
				}
			}
		}
		if failure == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return failure
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
