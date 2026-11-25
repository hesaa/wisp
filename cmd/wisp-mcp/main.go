// Command wisp-mcp is a lightweight stdio MCP server that forwards tool calls
// to the running Wisp desktop app over its local authenticated HTTP bridge.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"wa-go/internal/mcp"
)

const (
	mcpProtocolVersion = "2024-11-05"
	wispOfflineMessage = "Wisp is not running or MCP is disabled"
)

var bridgeClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	infoPath, err := mcp.DefaultInfoPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wisp-mcp: %v\n", err)
		os.Exit(1)
	}
	if err := serveStdio(os.Stdin, os.Stdout, infoPath); err != nil {
		fmt.Fprintf(os.Stderr, "wisp-mcp: %v\n", err)
		os.Exit(1)
	}
}

func serveStdio(r io.Reader, w io.Writer, infoPath string) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			if resp, ok := handleLine(context.Background(), trimmed, infoPath); ok {
				b, merr := json.Marshal(resp)
				if merr != nil {
					return merr
				}
				if _, werr := w.Write(append(b, '\n')); werr != nil {
					return werr
				}
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func handleLine(ctx context.Context, line []byte, infoPath string) (mcp.RPCResponse, bool) {
	var req mcp.RPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return mcp.RPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &mcp.RPCError{Code: -32700, Message: "invalid JSON: " + err.Error()},
		}, true
	}

	// JSON-RPC notifications (no id) and notifications/initialized produce no response.
	if req.Method == "notifications/initialized" || len(bytes.TrimSpace(req.ID)) == 0 {
		return mcp.RPCResponse{}, false
	}

	switch req.Method {
	case "initialize":
		return mcp.RPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "wisp",
					"version": "1.0.0",
				},
			},
		}, true

	case "ping":
		return mcp.RPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}, true

	case "tools/list":
		if resp, err := callBridge(ctx, infoPath, req); err == nil {
			resp.ID = req.ID
			return resp, true
		}
		// Fall back to static tool definitions when Wisp is offline so clients can register tools.
		return mcp.RPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": mcp.NewService(nil, nil).Tools()},
		}, true

	case "tools/call":
		resp, err := callBridge(ctx, infoPath, req)
		if err != nil {
			return mcp.RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcp.ToolResult{
					Content: []mcp.TextContent{{Type: "text", Text: wispOfflineMessage}},
					IsError: true,
				},
			}, true
		}
		resp.ID = req.ID
		return resp, true

	default:
		return mcp.RPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &mcp.RPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}, true
	}
}

func callBridge(ctx context.Context, infoPath string, req mcp.RPCRequest) (mcp.RPCResponse, error) {
	rawInfo, err := os.ReadFile(infoPath)
	if err != nil {
		return mcp.RPCResponse{}, err
	}
	var info mcp.BridgeInfo
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		return mcp.RPCResponse{}, err
	}
	if info.Port <= 0 || strings.TrimSpace(info.Token) == "" {
		return mcp.RPCResponse{}, errors.New("invalid bridge info")
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return mcp.RPCResponse{}, err
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/rpc", info.Port)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return mcp.RPCResponse{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+info.Token)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := bridgeClient.Do(httpReq)
	if err != nil {
		return mcp.RPCResponse{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return mcp.RPCResponse{}, fmt.Errorf("bridge status %d", httpResp.StatusCode)
	}

	var rpcResp mcp.RPCResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&rpcResp); err != nil {
		return mcp.RPCResponse{}, err
	}
	return rpcResp, nil
}
