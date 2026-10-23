package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
	log "github.com/sirupsen/logrus"

	"os"
	"path"
	"path/filepath"

	"gopkg.in/yaml.v2"
)

// LoadOrCreateConfig - Loads the config from disk or creates a new one
func LoadOrCreateConfig(altConfigLocation string, _appCallback AppCallback) (Config, error) {
	config, err := loadConfigFromDisk(altConfigLocation)

	if err != nil {
		if err == ErrFailedToFindConfigFile {
			log.Warn("No config file found, created default config file")
			config = defaultConfig()
		}
		if err == ErrInvalidConfigFile || err == ErrFailedToSaveConfig {
			return config, err
		}
	}
	if config.DirectClientAPIKey == "" {
		var token [24]byte
		if _, err := rand.Read(token[:]); err != nil {
			return config, fmt.Errorf("generate direct client key: %w", err)
		}
		config.DirectClientAPIKey = hex.EncodeToString(token[:])
	}

	// Override directory if running in docker
	if utils.IsRunningInDockerContainer() {
		// Override config data directories if blank
		if config.BlackholeDirectory == "" {
			log.Trace("Running in docker, overriding blank directory settings for blackhole directory to /blackhole inside the container")
			config.BlackholeDirectory = "/blackhole"
		}
		if config.DownloadsDirectory == "" {
			log.Trace("Running in docker, overriding blank directory settings for downloads directory to /downloads inside the container")
			config.DownloadsDirectory = "/downloads"
		}
	}

	log.Tracef("Setting config location to %s", altConfigLocation)

	config.appCallback = _appCallback
	config.altConfigLocation = altConfigLocation

	if err := config.Save(); err != nil {
		return config, err
	}

	return config, nil
}

// Save - Saves the config to disk. The new content goes to a 0600 temp
// file in the destination directory and is renamed over the previous
// config: a legacy config may still be world-readable (the tightening used
// to happen only after the write, so a freshly generated API key sat in a
// 0644 file in between), and a crash or error mid-save must not leave a
// truncated file where the last good config was.
func (c *Config) Save() error {
	log.Trace("Marshaling & saving config")
	data, err := yaml.Marshal(*c)
	if err != nil {
		log.Error(err)
		return err
	}

	savePath := "./config.yaml"
	if c.altConfigLocation != "" {
		savePath = path.Join(c.altConfigLocation, "config.yaml")
	}

	log.Tracef("Writing config to %s", savePath)
	tmp, err := os.CreateTemp(filepath.Dir(savePath), ".config-*.tmp")
	if err != nil {
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}
	tmpPath := tmp.Name()
	// Any failure before the rename leaves no debris behind.
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}
	if err := tmp.Close(); err != nil {
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}
	if err := os.Rename(tmpPath, savePath); err != nil {
		log.Errorf("Failed to save config file: %+v", err)
		return err
	}

	log.Trace("Config saved")
	return nil
}

func loadConfigFromDisk(altConfigLocation string) (Config, error) {
	var config Config

	log.Trace("Trying to load config from disk")
	configLocation := path.Join(altConfigLocation, "config.yaml")

	log.Tracef("Reading config from %s", configLocation)
	file, err := ioutil.ReadFile(configLocation)

	if err != nil {
		log.Trace("Failed to find config file")
		return config, ErrFailedToFindConfigFile
	}

	log.Trace("Loading to interface")
	var configInterface map[interface{}]interface{}
	err = yaml.Unmarshal(file, &configInterface)
	if err != nil {
		log.Errorf("Failed to unmarshal config file: %+v", err)
		return config, ErrInvalidConfigFile
	}

	log.Trace("Unmarshalling to struct")
	err = yaml.Unmarshal(file, &config)
	if err != nil {
		log.Errorf("Failed to unmarshal config file: %+v", err)
		return config, ErrInvalidConfigFile
	}

	log.Trace("Checking for missing config fields")
	updated := false

	if configInterface["Arrs"] == nil {
		log.Info("Arrs not set, setting to an empty list")
		config.Arrs = []ArrConfig{}
		updated = true
	}

	if configInterface["PollBlackholeDirectory"] == nil {
		log.Info("PollBlackholeDirectory not set, setting to false")
		config.PollBlackholeDirectory = false
		updated = true
	}

	if configInterface["SimultaneousDownloads"] == nil {
		log.Info("SimultaneousDownloads not set, setting to 5")
		config.SimultaneousDownloads = 5
		updated = true
	}
	// A hand-edited 0 (or a negative value) is kept as-is, not rewritten
	// to the default: every consumer reads a non-positive limit as
	// "no limit" (the transfer gate and the direct slot gate both treat
	// <= 0 as unlimited), so the literal keeps ONE meaning across load,
	// web save, and runtime. Rewriting it on load only would produce a
	// value the web save path never produces, so the same file would
	// mean "unlimited" after a save and "cap of 5" after a restart.

	if configInterface["DownloadSpeedLimit"] == nil {
		log.Info("DownloadSpeedLimit not set, setting to 100 Megabytes per second")
		config.DownloadSpeedLimit = 100
		updated = true
	}

	if configInterface["EnableTlsCheck"] == nil {
		log.Info("EnableTlsCheck not set, setting to false")
		config.EnableTlsCheck = false
		updated = true
	}

	if configInterface["TransferOnlyMode"] == nil {
		log.Info("TransferOnlyMode not set, setting to false")
		config.TransferOnlyMode = false
		updated = true
	}

	if configInterface["PollBlackholeIntervalMinutes"] == nil {
		log.Info("PollBlackholeIntervalMinutes not set, setting to 10")
		config.PollBlackholeIntervalMinutes = 10
		updated = true
	}

	if configInterface["ArrHistoryUpdateIntervalSeconds"] == nil {
		log.Info("ArrHistoryUpdateIntervalSeconds not set, setting to 20")
		config.ArrHistoryUpdateIntervalSeconds = 20
		updated = true
	}

	if configInterface["ErroredTransferDeleteGracePeriodSeconds"] == nil {
		log.Info("ErroredTransferDeleteGracePeriodSeconds not set, setting to 300")
		config.ErroredTransferDeleteGracePeriodSeconds = 300
		updated = true
	}

	config.altConfigLocation = altConfigLocation

	if updated {
		log.Trace("Version updated saving")
		err = config.Save()

		if err == nil {
			log.Trace("Config saved")
			return config, nil
		} else {
			log.Errorf("Failed to save config to %s", configLocation)
			log.Error(err)
			return config, ErrFailedToSaveConfig
		}
	}

	log.Trace("Config loaded")
	return config, nil
}

func defaultConfig() Config {
	return Config{
		PremiumizemeAPIKey: "xxxxxxxxx",
		Arrs: []ArrConfig{
			{Name: "Sonarr", URL: "http://127.0.0.1:8989", APIKey: "xxxxxxxxx", Type: Sonarr},
			{Name: "Radarr", URL: "http://127.0.0.1:7878", APIKey: "xxxxxxxxx", Type: Radarr},
			{Name: "Lidarr", URL: "http://127.0.0.1:8686", APIKey: "xxxxxxxxx", Type: Lidarr},
		},
		BlackholeDirectory:                      "",
		PollBlackholeDirectory:                  false,
		PollBlackholeIntervalMinutes:            10,
		DownloadsDirectory:                      "",
		TransferDirectory:                       "arrDownloads",
		BindIP:                                  "0.0.0.0",
		BindPort:                                "8182",
		WebRoot:                                 "",
		SimultaneousDownloads:                   5,
		DownloadSpeedLimit:                      100,
		EnableTlsCheck:                          false,
		TransferOnlyMode:                        false,
		ArrHistoryUpdateIntervalSeconds:         20,
		ErroredTransferDeleteGracePeriodSeconds: 300,
	}
}

var (
	ErrDownloadDirectorySetToRoot    = errors.New("download directory set to root")
	ErrDownloadDirectoryNotWriteable = errors.New("download directory not writeable")
)

func (c *Config) GetDownloadsBaseLocation() (string, error) {
	if c.DownloadsDirectory == "" {
		log.Tracef("Download directory not set, using default: %s", os.TempDir())
		return path.Join(os.TempDir(), "premiumizearrd"), nil
	}

	if c.DownloadsDirectory == "/" || c.DownloadsDirectory == "\\" || c.DownloadsDirectory == "C:\\" {
		log.Error("Download directory set to root, please set a directory")
		return "", ErrDownloadDirectorySetToRoot
	}

	if !utils.IsDirectoryWriteable(c.DownloadsDirectory) {
		log.Errorf("Download directory not writeable: %s", c.DownloadsDirectory)
		return c.DownloadsDirectory, ErrDownloadDirectoryNotWriteable
	}

	log.Tracef("Download directory set to: %s", c.DownloadsDirectory)
	return c.DownloadsDirectory, nil
}
