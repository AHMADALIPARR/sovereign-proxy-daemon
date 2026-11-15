package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		ListenAddr              string `yaml:"listen_addr"`
		RotationMode            string `yaml:"rotation_mode"`
		RotationIntervalSeconds int    `yaml:"rotation_interval_seconds"`
	} `yaml:"server"`

	HealthCheck struct {
		TargetURL       string `yaml:"target_url"`
		TimeoutSeconds  int    `yaml:"timeout_seconds"`
		IntervalSeconds int    `yaml:"interval_seconds"`
	} `yaml:"health_check"`

	UpstreamProxies []UpstreamProxy `yaml:"upstream_proxies"`
}

type UpstreamProxy struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
