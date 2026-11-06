package status

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
)

type config struct {
	ManagerURL         string `json:"manager_url"`
	ManagerKeyFile     string `json:"manager_key_file"`
	ReadTokenFile      string `json:"read_token_file"`
	ClientKeySHA256    string `json:"client_key_sha256"`
	SyncQuotaSnapshots bool   `json:"sync_quota_snapshots,omitempty"`
	EstimateStateFile  string `json:"estimate_state_file,omitempty"`
}

// 未配置环境覆盖时使用 CPA 的固定私有目录，以支持已有容器热加载。
// 显式设置但无效的覆盖值不能回退，避免误用另一套凭据。
func configurationPath() string {
	if path, present := os.LookupEnv("CPA_QUOTA_CONFIG"); present {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// 精简容器可能没有 HOME；只查询当前服务用户，不采用工作目录。
		account, lookupErr := user.Current()
		if lookupErr != nil || !filepath.IsAbs(account.HomeDir) {
			return ""
		}
		home = account.HomeDir
	}
	return filepath.Join(home, ".config", "CLIProxyAPI", "cpa-quota", "config.json")
}

func strictJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return errors.New("invalid_json")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid_json")
	}
	return nil
}

// 配置及秘密只允许从所有者私有的普通文件读取，不跟随最后一级符号链接。
func privateFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("private_file_required")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("private_file_unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || (limit > 0 && s.Size() > limit) {
		return nil, errors.New("private_file_invalid")
	}
	owner, ok := s.Sys().(*syscall.Stat_t)
	if !ok || (owner.Uid != uint32(os.Geteuid()) && owner.Uid != 0) {
		return nil, errors.New("private_file_owner_invalid")
	}
	var reader io.Reader = f
	if limit > 0 {
		reader = io.LimitReader(f, limit+1)
	}
	b, err := io.ReadAll(reader)
	if err != nil || (limit > 0 && int64(len(b)) > limit) {
		return nil, errors.New("private_file_invalid")
	}
	return b, nil
}

func validOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && ip != nil && ip.IsLoopback()
}

func loadConfig(path string) (config, string, []byte, error) {
	var c config
	raw, err := privateFile(path, 8192)
	if err != nil {
		return c, "", nil, err
	}
	if strictJSON(raw, &c) != nil || !validOrigin(c.ManagerURL) {
		return c, "", nil, errors.New("configuration_invalid")
	}
	if c.EstimateStateFile != "" && (!filepath.IsAbs(c.EstimateStateFile) ||
		filepath.Clean(c.EstimateStateFile) != c.EstimateStateFile ||
		c.EstimateStateFile == path || c.EstimateStateFile == c.ManagerKeyFile || c.EstimateStateFile == c.ReadTokenFile) {
		return c, "", nil, errors.New("estimate_state_path_invalid")
	}
	hash, err := hex.DecodeString(c.ClientKeySHA256)
	if err != nil || len(hash) != sha256.Size || c.ClientKeySHA256 != strings.ToLower(c.ClientKeySHA256) {
		return c, "", nil, errors.New("client_scope_invalid")
	}
	key, err := privateFile(c.ManagerKeyFile, 4096)
	if err != nil {
		return c, "", nil, err
	}
	token, err := privateFile(c.ReadTokenFile, 4096)
	if err != nil {
		return c, "", nil, err
	}
	key, token = bytes.TrimSpace(key), bytes.TrimSpace(token)
	validSecret := func(b []byte) bool {
		if len(b) == 0 || len(b) > 4096 {
			return false
		}
		for _, v := range b {
			if v < 33 || v > 126 {
				return false
			}
		}
		return true
	}
	if !validSecret(key) || !validSecret(token) || bytes.Equal(key, token) {
		return c, "", nil, errors.New("credential_invalid")
	}
	c.ManagerURL = strings.TrimRight(c.ManagerURL, "/")
	return c, string(key), token, nil
}
