package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"siyuan-worktree/internal/atomicfile"
)

const (
	MetadataDir = ".siyuan-worktree"
	ConfigFile  = "config.json"
	StateFile   = "state.json"
)

type Config struct {
	Version     int      `json:"version"`
	Endpoint    string   `json:"endpoint"`
	TokenEnv    string   `json:"tokenEnv"`
	NotebookIDs []string `json:"notebookIds"`
	OutputDir   string   `json:"outputDir"`
}

func Default() Config {
	return Config{
		Version:     1,
		Endpoint:    "http://127.0.0.1:6806",
		TokenEnv:    "SIYUAN_TOKEN",
		NotebookIDs: []string{},
		OutputDir:   "notes",
	}
}

func Init(root string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	configPath := Path(root)
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("mapping workspace is already initialized: %s", configPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat config: %w", err)
	}
	if err := os.MkdirAll(OutputPath(root, cfg), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return atomicfile.WriteJSON(configPath, cfg, 0o600)
}

func Load(root string) (Config, error) {
	data, err := os.ReadFile(Path(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("no mapping workspace at %s; run init first", root)
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return errors.New("endpoint must be an absolute http or https URL")
	}
	if endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("endpoint must not contain a query or fragment")
	}
	if c.TokenEnv == "" {
		return errors.New("tokenEnv must not be empty")
	}
	if c.OutputDir == "" || filepath.IsAbs(c.OutputDir) {
		return errors.New("outputDir must be a non-empty relative path")
	}
	cleanOutput := filepath.Clean(c.OutputDir)
	if cleanOutput == ".." || strings.HasPrefix(cleanOutput, ".."+string(filepath.Separator)) {
		return errors.New("outputDir must stay inside the mapping workspace")
	}
	return nil
}

func Path(root string) string {
	return filepath.Join(root, MetadataDir, ConfigFile)
}

func StatePath(root string) string {
	return filepath.Join(root, MetadataDir, StateFile)
}

func OutputPath(root string, cfg Config) string {
	return filepath.Join(root, filepath.FromSlash(cfg.OutputDir))
}
