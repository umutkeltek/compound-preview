package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxCodexAuthBytes = 64 << 10

var errCodexSubscriptionUnavailable = errors.New("codex subscription binding: private ChatGPT auth unavailable or invalid")

func validateCodexSubscriptionInvocation(in RunInput) error {
	if in.PermissionMode != "" && in.PermissionMode != "plan" {
		return fmt.Errorf("codex subscription binding: read-only permission mode required")
	}
	if filepath.Base(strings.TrimSpace(in.Command)) != "codex" {
		return fmt.Errorf("codex subscription binding: native codex executable required for read-only enforcement")
	}
	for _, item := range in.Env {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if key == "CODEX_HOME" || isCodexSubscriptionAPIEnv(key) {
			return fmt.Errorf("codex subscription binding: caller auth environment override refused")
		}
	}
	return nil
}

func codexNativeSelection(in RunInput) (string, string, error) {
	model := strings.TrimSpace(in.RequestedModelID)
	effort := ""
	if snap := in.ExecutionSelectionSnapshot; snap != nil {
		if model != "" && snap.RequestedModelID != "" && model != snap.RequestedModelID {
			return "", "", fmt.Errorf("codex adapter: selected model identities disagree")
		}
		if model == "" {
			model = strings.TrimSpace(snap.RequestedModelID)
		}
		effort = strings.TrimSpace(snap.RequestedEffort)
	}
	if model != "" {
		if len(model) > 64 || !(strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")) {
			return "", "", fmt.Errorf("codex adapter: selected model is not a native Codex model id")
		}
		for _, ch := range model {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.' || ch == '_') {
				return "", "", fmt.Errorf("codex adapter: selected model is not a native Codex model id")
			}
		}
	}
	switch effort {
	case "", "low", "medium", "high", "xhigh":
	default:
		return "", "", fmt.Errorf("codex adapter: requested effort unsupported by native Codex")
	}
	return model, effort, nil
}

func isCodexSubscriptionAPIEnv(key string) bool {
	return strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "AZURE_OPENAI_") || key == "CODEX_API_KEY"
}

func codexSubscriptionChildEnv(extra []string, isolatedHome string) []string {
	base := append([]string(nil), os.Environ()...)
	base = append(base, extra...)
	out := make([]string, 0, len(base)+1)
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok && (key == "CODEX_HOME" || key == "COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION" || isCodexSubscriptionAPIEnv(key)) {
			continue
		}
		out = append(out, item)
	}
	return append(out, "CODEX_HOME="+isolatedHome)
}

func writeCodexSubscriptionSnapshot(isolatedHome string) error {
	sourceHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if sourceHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return errCodexSubscriptionUnavailable
		}
		sourceHome = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(sourceHome, "auth.json")
	meta, err := os.Lstat(path)
	if err != nil || !meta.Mode().IsRegular() || meta.Size() <= 0 || meta.Size() > maxCodexAuthBytes || meta.Mode().Perm()&0o077 != 0 {
		return errCodexSubscriptionUnavailable
	}
	owner, ok := meta.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return errCodexSubscriptionUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return errCodexSubscriptionUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(meta, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return errCodexSubscriptionUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCodexAuthBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxCodexAuthBytes {
		return errCodexSubscriptionUnavailable
	}
	var source struct {
		AuthMode    string          `json:"auth_mode"`
		APIKey      json.RawMessage `json:"OPENAI_API_KEY"`
		LastRefresh json.RawMessage `json:"last_refresh"`
		Tokens      struct {
			Access  string `json:"access_token"`
			ID      string `json:"id_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &source) != nil || source.AuthMode != "chatgpt" ||
		(len(source.APIKey) > 0 && string(source.APIKey) != "null") ||
		strings.TrimSpace(source.Tokens.Access) == "" || strings.TrimSpace(source.Tokens.ID) == "" || strings.TrimSpace(source.Tokens.Account) == "" {
		return errCodexSubscriptionUnavailable
	}
	snapshot := struct {
		AuthMode    string          `json:"auth_mode"`
		APIKey      any             `json:"OPENAI_API_KEY"`
		LastRefresh json.RawMessage `json:"last_refresh,omitempty"`
		Tokens      struct {
			Access  string `json:"access_token"`
			ID      string `json:"id_token"`
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}{AuthMode: "chatgpt", LastRefresh: source.LastRefresh}
	snapshot.Tokens.Access = source.Tokens.Access
	snapshot.Tokens.ID = source.Tokens.ID
	snapshot.Tokens.Account = source.Tokens.Account
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return errCodexSubscriptionUnavailable
	}
	dest := filepath.Join(isolatedHome, "auth.json")
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errCodexSubscriptionUnavailable
	}
	_, writeErr := out.Write(encoded)
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil {
		return errCodexSubscriptionUnavailable
	}
	return nil
}
