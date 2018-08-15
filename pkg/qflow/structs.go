package qflow

import (
	"io/ioutil"
	"time"

	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	HTTP struct {
		Timeout time.Duration `yaml:"timeout"`
	}

	Endpoints []struct {
		Name  string   `yaml:"name"`
		Hosts []string `yaml:"hosts"`
	}
}

// type UnmarshalingTimeout time.Duration

// ParseConfig handles mapping the filename to config struct
func ParseConfig(filename string) (*Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	config := Config{}
	err = yaml.Unmarshal([]byte(data), &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}