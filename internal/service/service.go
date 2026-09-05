package service

import (
	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

// Service interface
type Service interface {
	New() (*config.Config, error)
	Start() error
	Stop() error
}
