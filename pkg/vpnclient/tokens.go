package vpnclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

const (
	legacyTokenFileName = ".firefox-vpn-tokens.json"
	ffvDirName          = ".ffv"
	foxyvpnDirName      = ".foxyvpn"
	ffvTokenName        = "tokens.json"
)

type SavedTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scope        string    `json:"scope"`
	ObtainedAt   time.Time `json:"obtained_at"`
	ExpiresIn    int       `json:"expires_in"`
}

func (s *SavedTokens) AccessTokenValid() bool {
	if s.AccessToken == "" {
		return false
	}
	expiry := s.ObtainedAt.Add(time.Duration(s.ExpiresIn) * time.Second)
	return time.Now().Before(expiry.Add(-60 * time.Second))
}

func candidateHomeDirs() []string {
	var homes []string
	seen := make(map[string]bool)

	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		homes = append(homes, dir)
	}

	// 1. Current user's home
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		add(h)
	}

	// 2. If running under sudo (e.g. sudo ffv on Linux/macOS), check the original user's home!
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			add(u.HomeDir)
		}
		add(filepath.Join("/home", sudoUser))
		add(filepath.Join("/Users", sudoUser))
	}

	return homes
}

func TokenFilePath() string {
	if explicit := os.Getenv("FFV_TOKEN_FILE"); explicit != "" {
		return explicit
	}
	if explicit := os.Getenv("FOXYVPN_TOKEN_FILE"); explicit != "" {
		return explicit
	}

	homes := candidateHomeDirs()

	// 1. Search for existing token file (prefer .ffv, then .foxyvpn fallback)
	for _, h := range homes {
		ffvPath := filepath.Join(h, ffvDirName, ffvTokenName)
		if _, err := os.Stat(ffvPath); err == nil {
			return ffvPath
		}
		foxyPath := filepath.Join(h, foxyvpnDirName, ffvTokenName)
		if _, err := os.Stat(foxyPath); err == nil {
			return foxyPath
		}
		legacyPath := filepath.Join(h, legacyTokenFileName)
		if _, err := os.Stat(legacyPath); err == nil {
			return legacyPath
		}
	}

	// 2. Fallback: if running as sudo, prefer sudo user's home
	if len(homes) > 1 {
		return filepath.Join(homes[1], ffvDirName, ffvTokenName)
	}
	if len(homes) > 0 {
		return filepath.Join(homes[0], ffvDirName, ffvTokenName)
	}
	return legacyTokenFileName
}

func primaryTokenPath() string {
	if explicit := os.Getenv("FFV_TOKEN_FILE"); explicit != "" {
		return explicit
	}
	if explicit := os.Getenv("FOXYVPN_TOKEN_FILE"); explicit != "" {
		return explicit
	}
	homes := candidateHomeDirs()
	if len(homes) > 1 {
		return filepath.Join(homes[1], ffvDirName, ffvTokenName)
	}
	if len(homes) > 0 {
		return filepath.Join(homes[0], ffvDirName, ffvTokenName)
	}
	return legacyTokenFileName
}

func LoadTokens() (*SavedTokens, error) {
	path := TokenFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tokens SavedTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func SaveTokens(tok *TokenResponse) error {
	saved := SavedTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Scope:        tok.Scope,
		ObtainedAt:   time.Now(),
		ExpiresIn:    tok.ExpiresIn,
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	path := primaryTokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	if err := writePrivateFile(path, data); err != nil {
		return fmt.Errorf("saving tokens to %s: %w", path, err)
	}

	// If running under sudo, ensure original user also has ownership
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := user.Lookup(sudoUser); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				if gid, err := strconv.Atoi(u.Gid); err == nil {
					_ = os.Chown(filepath.Dir(path), uid, gid)
					_ = os.Chown(path, uid, gid)
				}
			}
		}
	}

	return nil
}

func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		if retryErr := os.Rename(tmpPath, path); retryErr != nil {
			return retryErr
		}
	}
	return os.Chmod(path, 0600)
}

func DeleteTokens() {
	for _, home := range candidateHomeDirs() {
		_ = os.Remove(filepath.Join(home, ffvDirName, ffvTokenName))
		_ = os.Remove(filepath.Join(home, foxyvpnDirName, ffvTokenName))
		_ = os.Remove(filepath.Join(home, legacyTokenFileName))
	}
}
