package cli

import (
	"gopkg.in/yaml.v3"
	"os"
)

type Config struct {
	Name     string `yaml:"name"`
	Domain   string `yaml:"domain"`
	Web      WebCfg `yaml:"web"`
	Database DBCfg  `yaml:"database"`
	Services struct {
		Redis   bool `yaml:"redis"`
		Mailpit bool `yaml:"mailpit"`
		Adminer bool `yaml:"adminer"`
	} `yaml:"services"`
	Xdebug struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"xdebug"`
	Perf struct {
		Sync     string   `yaml:"sync"`
		Excludes []string `yaml:"excludes"`
	} `yaml:"perf"`
	TLS TLSCfg `yaml:"tls"`
}

type WebCfg struct {
	Server  string `yaml:"server"` // nginx|apache
	PHP     string `yaml:"php"`
	Docroot string `yaml:"docroot"`
}

type DBCfg struct {
	Engine      string `yaml:"engine"`
	Version     string `yaml:"version"`
	Portforward string `yaml:"portforward"`
	Persist     string `yaml:"persist"`
  	DataPath    string `yaml:"data_path"`
}

type TLSCfg struct {
	Enabled bool `yaml:"enabled"` // on/off (mkcert when true)
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil { return &Config{}, nil } // allow missing (init)
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil { return nil, err }
	return &cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil { return err }
	return os.WriteFile(path, b, 0o644)
}
