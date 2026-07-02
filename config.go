package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"log"
	"os"
	"path"
	"time"

	"github.com/insomniacslk/editor"
	"github.com/kirsle/configdir"
	"gopkg.in/yaml.v3"
)

// configTemplate is written to the config file on first run, before opening
// it in the editor.
//
//go:embed config.yaml.example
var configTemplate []byte

// DeviceConfig identifies a Tapo device to control.
type DeviceConfig struct {
	// Name is the label shown in the tray menu. If empty, the device's
	// nickname is used once it has been fetched from the device.
	Name string `yaml:"name"`
	// Address is the IP address or host name of the device.
	Address string `yaml:"address"`
	// Type is the device kind, "plug" (the default) or "bulb".
	Type string `yaml:"type"`
}

func (dc *DeviceConfig) kind() deviceKind {
	if dc.Type == "bulb" {
		return kindBulb
	}
	return kindPlug
}

// Config contains the program's configuration.
type Config struct {
	Email          string         `yaml:"email"`
	Password       string         `yaml:"password"`
	UpdateInterval string         `yaml:"update_interval"`
	updateInterval time.Duration  `yaml:"-"`
	Discover       bool           `yaml:"discover"`
	CloudList      bool           `yaml:"cloud_list"`
	Debug          bool           `yaml:"debug"`
	Devices        []DeviceConfig `yaml:"devices"`
}

// tapoLogger returns the logger passed to the tapo library. The library
// falls back to a discard logger when nil.
func (c *Config) tapoLogger() *log.Logger {
	if c.Debug {
		return log.New(os.Stderr, "tapo: ", log.LstdFlags)
	}
	return nil
}

// configFilePaths returns the configuration directory and the configuration
// file path within it.
func configFilePaths() (string, string) {
	configPath := configdir.LocalConfig(progname)
	return configPath, path.Join(configPath, "config.yaml")
}

// editConfig opens the config file in the default editor, creating it from
// the template if it does not exist, then validates the result.
func editConfig() error {
	configPath, configFile := configFilePaths()
	if err := configdir.MakePath(configPath); err != nil {
		return fmt.Errorf("failed to create config path %q: %w", configPath, err)
	}
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.WriteFile(configFile, configTemplate, 0600); err != nil {
			return fmt.Errorf("failed to create config file: %w", err)
		}
	}
	if err := editor.Open(configFile); err != nil {
		return fmt.Errorf("failed to open config file in editor: %w", err)
	}
	if _, _, err := loadConfig(); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}
	fmt.Printf("Configuration file %q is valid\n", configFile)
	return nil
}

func loadConfig() (string, *Config, error) {
	cfg := Config{}

	configPath, configFile := configFilePaths()
	log.Printf("Trying to load config file %q", configFile)
	if err := configdir.MakePath(configPath); err != nil {
		if os.IsNotExist(err) {
			return configFile, &cfg, nil
		}
		return configFile, nil, fmt.Errorf("failed to create config path %q: %w", configPath, err)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			// create a template file and open it with the default editor.
			// 0600 because the file contains credentials.
			if err := os.WriteFile(configFile, configTemplate, 0600); err != nil {
				return configFile, &cfg, fmt.Errorf("failed to create config file: %w", err)
			}
			fmt.Printf("Configuration file %q does not exist, a template has been created.\n", configFile)
			fmt.Println("An editor will now open it: replace the template values with your own settings, then save and close the editor.")
			fmt.Print("Press Enter to open the editor...")
			// ignore the error: when stdin is not available (e.g. launched
			// from a desktop environment), just proceed to open the editor
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			if err := editor.Open(configFile); err != nil {
				return configFile, &cfg, fmt.Errorf("failed to open config file in editor: %w", err)
			}
			// re-read the config file
			data, err = os.ReadFile(configFile)
			if err != nil {
				return configFile, &cfg, fmt.Errorf("failed to read config file: %w", err)
			}
			// after this point, the newly created config file will be parsed by
			// the rest of this function.
		} else {
			return configFile, nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return configFile, nil, fmt.Errorf("failed to unmarshal config file: %w", err)
	}

	// sanity checks
	if cfg.Email == "" {
		return configFile, nil, fmt.Errorf("email cannot be empty")
	}
	if cfg.Password == "" {
		return configFile, nil, fmt.Errorf("password cannot be empty")
	}
	if cfg.Discover && cfg.CloudList {
		return configFile, nil, fmt.Errorf("discover and cloud_list are mutually exclusive, set at most one")
	}
	if len(cfg.Devices) == 0 && !cfg.Discover && !cfg.CloudList {
		return configFile, nil, fmt.Errorf("no devices configured and no device source enabled: set devices, discover, or cloud_list")
	}
	for idx, d := range cfg.Devices {
		if d.Address == "" {
			return configFile, nil, fmt.Errorf("device #%d: address cannot be empty", idx+1)
		}
		if d.Type != "" && d.Type != "plug" && d.Type != "bulb" {
			return configFile, nil, fmt.Errorf("device #%d: type must be \"plug\" or \"bulb\", got %q", idx+1, d.Type)
		}
	}
	if cfg.UpdateInterval == "" {
		return configFile, nil, fmt.Errorf("update_interval cannot be empty")
	}
	ui, err := time.ParseDuration(cfg.UpdateInterval)
	if err != nil {
		return configFile, nil, fmt.Errorf("update_interval is not a valid duration string: %w", err)
	}
	cfg.updateInterval = ui

	return configFile, &cfg, nil
}
