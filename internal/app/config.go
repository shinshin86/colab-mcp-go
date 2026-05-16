package app

import "time"

const Version = "v0.1.0"

type Config struct {
	LogDir         string
	Host           string
	ConnectTimeout time.Duration
	NoBrowser      bool
	EnableProxy    bool
}

func DefaultConfig() Config {
	return Config{
		Host:           "localhost",
		ConnectTimeout: 60 * time.Second,
		EnableProxy:    true,
	}
}
