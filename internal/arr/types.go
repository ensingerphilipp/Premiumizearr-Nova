package arr

import (
	"strings"
	"sync"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	"golift.io/starr/lidarr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

// grabbedEventType is the *arr history event type (camelCase in Sonarr,
// Radarr and Lidarr) of a release grab. Only grabbed records can be marked
// failed in the *arr, so error reporting must resolve to one of them.
const grabbedEventType = "grabbed"

func CompareFileNamesFuzzy(a, b string) bool {
	//Strip file extension
	a = utils.StripDownloadTypesExtention(a)
	b = utils.StripDownloadTypesExtention(b)
	//Strip media type extension
	a = utils.StripMediaTypesExtention(a)
	b = utils.StripMediaTypesExtention(b)
	//Strip Spaces
	a = strings.ReplaceAll(a, " ", "")
	b = strings.ReplaceAll(b, " ", "")
	//Strip periods
	a = strings.ReplaceAll(a, ".", "")
	b = strings.ReplaceAll(b, ".", "")
	//Strip dashes
	a = strings.ReplaceAll(a, "-", "")
	b = strings.ReplaceAll(b, "-", "")
	//Strip underscores
	a = strings.ReplaceAll(a, "_", "")
	b = strings.ReplaceAll(b, "_", "")
	//Convert to lowercase
	a = strings.ToLower(a)
	b = strings.ToLower(b)

	return a == b
}

type IArr interface {
	// HistoryContains reports whether the arr's history contains a grabbed
	// record fuzzy-matching name. A non-nil error means the history could
	// not be retrieved (e.g. the arr is unreachable) and must NOT be
	// treated as an authoritative no-match. (id, true, nil) is a match;
	// (-1, false, nil) is an authoritative no-match.
	HistoryContains(name string) (id int64, found bool, err error)
	MarkHistoryItemAsFailed(int64) error
	HandleErrorTransfer(*premiumizeme.Transfer, int64, *premiumizeme.Premiumizeme) error
	GetArrName() string
}

type SonarrArr struct {
	Name                 string
	ClientMutex          sync.Mutex
	Client               *sonarr.Sonarr
	HistoryMutex         sync.Mutex
	History              *sonarr.History
	LastUpdateMutex      sync.Mutex
	LastUpdate           time.Time
	LastUpdateCount      int
	LastUpdateCountMutex sync.Mutex
	Config               *config.Config
}

type RadarrArr struct {
	Name                 string
	ClientMutex          sync.Mutex
	Client               *radarr.Radarr
	HistoryMutex         sync.Mutex
	History              *radarr.History
	LastUpdateMutex      sync.Mutex
	LastUpdate           time.Time
	LastUpdateCount      int
	LastUpdateCountMutex sync.Mutex
	Config               *config.Config
}

type LidarrArr struct {
	Name                 string
	ClientMutex          sync.Mutex
	Client               *lidarr.Lidarr
	HistoryMutex         sync.Mutex
	History              *lidarr.History
	LastUpdateMutex      sync.Mutex
	LastUpdate           time.Time
	LastUpdateCount      int
	LastUpdateCountMutex sync.Mutex
	Config               *config.Config
}
