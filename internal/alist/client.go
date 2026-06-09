// Package alist provides a client for the AList v3 REST API.
package alist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client interacts with an AList server.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New creates a client and authenticates with username/password.
func New(baseURL, username, password string) (*Client, error) {
	c := &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	if err := c.login(context.Background(), username, password); err != nil {
		return nil, fmt.Errorf("alist login: %w", err)
	}
	return c, nil
}

func (c *Client) login(ctx context.Context, username, password string) error {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/auth/login", body, &resp); err != nil {
		return err
	}
	if resp.Code != 200 {
		return fmt.Errorf("login failed (code %d): %s", resp.Code, resp.Msg)
	}
	c.token = resp.Data.Token
	return nil
}

// AddLocalStorage creates an AList Local-driver storage that points to a path
// already mounted inside the AList pod (via CSI SMB PVC).
//
//   mountPath   – virtual path in AList UI, e.g. "/server-a-d"
//   localRoot   – absolute path inside the AList container, e.g. "/mnt/server-a-d"
func (c *Client) AddLocalStorage(ctx context.Context, mountPath, localRoot string) (int, error) {
	addition, _ := json.Marshal(map[string]any{
		"root_folder_path": localRoot,
		"thumbnail":        false,
		"show_hidden":      false,
		"mkdir_perm":       "022",
	})
	payload := map[string]any{
		"mount_path":       mountPath,
		"driver":           "Local",
		"order":            0,
		"remark":           "",
		"cache_expiration": 30,
		"web_proxy":        false,
		"enable_sign":      false,
		"addition":         string(addition),
	}
	body, _ := json.Marshal(payload)

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/admin/storage/create", body, &resp); err != nil {
		return 0, err
	}
	if resp.Code != 200 {
		return 0, fmt.Errorf("add storage failed (code %d): %s", resp.Code, resp.Msg)
	}
	return resp.Data.ID, nil
}

// EnsureGuestRead enables AList's built-in "guest" user so that the storages
// can be browsed and downloaded without logging in. AList ships the guest user
// disabled by default; this flips it on (read-only, base path "/") while
// preserving every other field of the user object. It is idempotent: if guest
// is already enabled it does nothing.
func (c *Client) EnsureGuestRead(ctx context.Context) error {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Content []map[string]any `json:"content"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/admin/user/list?page=1&per_page=100", &resp); err != nil {
		return err
	}
	if resp.Code != 200 {
		return fmt.Errorf("list users (code %d): %s", resp.Code, resp.Msg)
	}

	var guest map[string]any
	for _, u := range resp.Data.Content {
		if name, _ := u["username"].(string); name == "guest" {
			guest = u
			break
		}
	}
	if guest == nil {
		return fmt.Errorf("guest user not found in AList")
	}

	// Already enabled → nothing to do.
	if dis, ok := guest["disabled"].(bool); ok && !dis {
		return nil
	}

	guest["disabled"] = false
	if bp, _ := guest["base_path"].(string); bp == "" {
		guest["base_path"] = "/"
	}

	body, _ := json.Marshal(guest)
	var up struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := c.post(ctx, "/api/admin/user/update", body, &up); err != nil {
		return err
	}
	if up.Code != 200 {
		return fmt.Errorf("enable guest (code %d): %s", up.Code, up.Msg)
	}
	return nil
}

// StorageInfo holds minimal storage info from AList.
type StorageInfo struct {
	ID        int    `json:"id"`
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver"`
	Status    string `json:"status"`
}

// ListStorages returns all configured storages.
func (c *Client) ListStorages(ctx context.Context) ([]StorageInfo, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Content []StorageInfo `json:"content"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/admin/storage/list?page=1&per_page=100", &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("list storages (code %d): %s", resp.Code, resp.Msg)
	}
	return resp.Data.Content, nil
}

// DeleteStorage removes a storage by ID.
func (c *Client) DeleteStorage(ctx context.Context, id int) error {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := c.post(ctx, fmt.Sprintf("/api/admin/storage/delete?id=%d", id), nil, &resp); err != nil {
		return err
	}
	if resp.Code != 200 {
		return fmt.Errorf("delete storage (code %d): %s", resp.Code, resp.Msg)
	}
	return nil
}

// FileURL builds the AList direct-download URL for a file.
// baseURL is the AList server base (e.g. http://alist.middleware.svc.cluster.local:5244).
// mountPath is the AList virtual path (e.g. /server-a-d).
// relPath is the relative path under the mount (e.g. drawings/file.pdf).
func FileURL(baseURL, mountPath, relPath string) string {
	rel := strings.TrimLeft(relPath, "/\\")
	rel = strings.ReplaceAll(rel, `\`, "/")
	return strings.TrimRight(baseURL, "/") + "/d" + mountPath + "/" + rel
}

func (c *Client) post(ctx context.Context, path string, body []byte, out any) error {
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader([]byte{})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
