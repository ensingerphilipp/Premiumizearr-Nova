package config

import "errors"

var (
	ErrInvalidConfigFile      = errors.New("invalid Config File")
	ErrFailedToFindConfigFile = errors.New("failed to find config file")
	ErrFailedToSaveConfig     = errors.New("failed to save config")
)

// ArrType enum for Sonarr/Radarr/Lidarr
type ArrType string

// AppCallback - Callback for the app to use
type AppCallback func(oldConfig Config, newConfig Config)

const (
	Sonarr ArrType = "Sonarr"
	Radarr ArrType = "Radarr"
	Lidarr ArrType = "Lidarr"
)

type ArrConfig struct {
	Name   string  `yaml:"Name" json:"Name"`
	URL    string  `yaml:"URL" json:"URL"`
	APIKey string  `yaml:"APIKey" json:"APIKey"`
	Type   ArrType `yaml:"Type" json:"Type"`
}

type Config struct {
	altConfigLocation string
	appCallback       AppCallback

	//PremiumizemeAPIKey string with yaml and json tag
	PremiumizemeAPIKey string `yaml:"PremiumizemeAPIKey" json:"PremiumizemeAPIKey"`

	Arrs []ArrConfig `yaml:"Arrs" json:"Arrs"`

	BlackholeDirectory           string `yaml:"BlackholeDirectory" json:"BlackholeDirectory"`
	PollBlackholeDirectory       bool   `yaml:"PollBlackholeDirectory" json:"PollBlackholeDirectory"`
	PollBlackholeIntervalMinutes int    `yaml:"PollBlackholeIntervalMinutes" json:"PollBlackholeIntervalMinutes"`

	DownloadsDirectory string `yaml:"DownloadsDirectory" json:"DownloadsDirectory"`
	TransferDirectory  string `yaml:"TransferDirectory" json:"TransferDirectory"`

	BindIP   string `yaml:"bindIP" json:"BindIP"`
	BindPort string `yaml:"bindPort" json:"BindPort"`

	WebRoot string `yaml:"WebRoot" json:"WebRoot"`

	SimultaneousDownloads int  `yaml:"SimultaneousDownloads" json:"SimultaneousDownloads"`
	DownloadSpeedLimit    int  `yaml:"DownloadSpeedLimit" json:"DownloadSpeedLimit"`
	EnableTlsCheck        bool `yaml:"EnableTlsCheck" json:"EnableTlsCheck"`

	ArrHistoryUpdateIntervalSeconds int `yaml:"ArrHistoryUpdateIntervalSeconds" json:"ArrHistoryUpdateIntervalSeconds"`
}
