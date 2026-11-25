package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	rpcErrorParse          = -32700
	rpcErrorInvalidRequest = -32600
	rpcErrorMethodNotFound = -32601
	rpcErrorInvalidParams  = -32602
)

// BridgeInfo describes the running local HTTP MCP bridge stored in mcp.json.
type BridgeInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// RPCRequest is a JSON-RPC 2.0 request envelope.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RPCResponse is a JSON-RPC 2.0 response envelope.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Bridge exposes a Service over an authenticated loopback HTTP JSON-RPC endpoint
// and writes discovery info (port + bearer token) to mcp.json while running.
type Bridge struct {
	svc      *Service
	infoPath string

	mu     sync.Mutex
	srv    *http.Server
	token  string
	active string
}

// NewBridge creates a Bridge backed by svc that writes connection info to infoPath.
// If infoPath is empty, Start resolves DefaultInfoPath().
func NewBridge(svc *Service, infoPath string) *Bridge {
	return &Bridge{
		svc:      svc,
		infoPath: infoPath,
	}
}

// DefaultInfoPath returns %LOCALAPPDATA%\Wisp\mcp.json.
func DefaultInfoPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("mcp: resolve cache dir: %w", err)
	}
	return filepath.Join(base, "Wisp", "mcp.json"), nil
}

// DefaultClientConfigSnippet returns a ready-to-paste mcpServers JSON snippet for exePath.
func DefaultClientConfigSnippet(exePath string) string {
	if strings.TrimSpace(exePath) == "" {
		exePath = "wisp-mcp.exe"
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"wisp": map[string]any{
				"command": exePath,
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

// Start binds 127.0.0.1:0, writes mcp.json with a random bearer token, and starts serving /rpc.
func (b *Bridge) Start() error {
	if b.svc == nil {
		return errors.New("mcp: service is nil")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.srv != nil {
		return errors.New("mcp: bridge already started")
	}

	infoPath := b.infoPath
	if infoPath == "" {
		var err error
		infoPath, err = DefaultInfoPath()
		if err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(infoPath), 0o700); err != nil {
		return fmt.Errorf("mcp: create info dir: %w", err)
	}

	token, err := randomHexToken(32)
	if err != nil {
		return fmt.Errorf("mcp: generate token: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("mcp: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	infoBytes, err := json.MarshalIndent(BridgeInfo{Port: port, Token: token}, "", "  ")
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("mcp: marshal bridge info: %w", err)
	}
	if err := writeFileAtomic(infoPath, infoBytes, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("mcp: write %s: %w", infoPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", b.handleRPC(token))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	b.srv = srv
	b.token = token
	b.active = infoPath

	go func() {
		_ = srv.Serve(ln)
	}()

	return nil
}

// Stop shuts down the HTTP bridge and removes mcp.json.
func (b *Bridge) Stop() error {
	b.mu.Lock()
	srv := b.srv
	infoPath := b.active
	b.srv = nil
	b.token = ""
	b.active = ""
	b.mu.Unlock()

	var errs []error
	if infoPath != "" {
		if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("mcp: remove %s: %w", infoPath, err))
		}
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			if closeErr := srv.Close(); closeErr != nil {
				errs = append(errs, fmt.Errorf("mcp: close server: %w", closeErr))
			}
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) handleRPC(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Connection", "close")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizedBearer(r.Header.Get("Authorization"), token) {
			w.Header().Set("Connection", "close")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		var req RPCRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			writeRPC(w, RPCResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &RPCError{Code: rpcErrorParse, Message: "invalid JSON: " + err.Error()},
			})
			return
		}

		id := req.ID
		if len(bytes.TrimSpace(id)) == 0 {
			id = json.RawMessage("null")
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			writeRPC(w, RPCResponse{
				JSONRPC: "2.0",
				ID:      id,
				Error:   &RPCError{Code: rpcErrorInvalidRequest, Message: "jsonrpc must be 2.0"},
			})
			return
		}

		switch req.Method {
		case "tools/list":
			writeRPC(w, RPCResponse{
				JSONRPC: "2.0",
				ID:      id,
				Result:  map[string]any{"tools": b.svc.Tools()},
			})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if len(req.Params) > 0 {
				if err := json.Unmarshal(req.Params, &params); err != nil {
					writeRPC(w, RPCResponse{
						JSONRPC: "2.0",
						ID:      id,
						Error:   &RPCError{Code: rpcErrorInvalidParams, Message: "invalid params: " + err.Error()},
					})
					return
				}
			}
			if strings.TrimSpace(params.Name) == "" {
				writeRPC(w, RPCResponse{
					JSONRPC: "2.0",
					ID:      id,
					Error:   &RPCError{Code: rpcErrorInvalidParams, Message: "tool name is required"},
				})
				return
			}
			res, err := b.svc.CallTool(r.Context(), params.Name, params.Arguments)
			if err != nil {
				writeRPC(w, RPCResponse{
					JSONRPC: "2.0",
					ID:      id,
					Error:   &RPCError{Code: rpcErrorInvalidParams, Message: err.Error()},
				})
				return
			}
			writeRPC(w, RPCResponse{
				JSONRPC: "2.0",
				ID:      id,
				Result:  res,
			})
		default:
			writeRPC(w, RPCResponse{
				JSONRPC: "2.0",
				ID:      id,
				Error:   &RPCError{Code: rpcErrorMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)},
			})
		}
	}
}

func authorizedBearer(header, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := header[len(prefix):]
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeRPC(w http.ResponseWriter, resp RPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mcp-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
