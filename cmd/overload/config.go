package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const configFileName = "overload.env"

func defaultHome() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "overload"), nil
}

// configPath returns OVERLOAD_CONFIG, or the default install config file.
func configPath() string {
	if path := os.Getenv("OVERLOAD_CONFIG"); path != "" {
		return path
	}
	home, err := defaultHome()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configFileName)
}

// loadConfig sets variables from the env file that are not already set in the
// environment, so explicit environment variables always win. A missing file
// is not an error.
func loadConfig(path string) error {
	if path == "" {
		return nil
	}
	values, err := readEnvFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for key, value := range values {
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func writeEnvFile(path string, values map[string]string, order []string) error {
	var content strings.Builder
	content.WriteString("# Written by overload install. Environment variables override these values.\n")
	for _, key := range order {
		if value, ok := values[key]; ok {
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("%s contains a line break", key)
			}
			fmt.Fprintf(&content, "%s=%s\n", key, value)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(content.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
