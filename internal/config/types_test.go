package config

import "testing"

func TestValidateArrs(t *testing.T) {
	tests := []struct {
		name    string
		arrs    []ArrConfig
		wantErr bool
	}{
		{"empty list", []ArrConfig{}, false},
		{"valid single slug", []ArrConfig{{Name: "sonarr"}}, false},
		{"valid slug with hyphen", []ArrConfig{{Name: "sonarr-anime"}}, false},
		{"valid slug with digits", []ArrConfig{{Name: "radarr-4k"}}, false},
		{"valid distinct slugs", []ArrConfig{{Name: "sonarr"}, {Name: "radarr"}}, false},
		{"uppercase rejected", []ArrConfig{{Name: "Sonarr"}}, true},
		{"space rejected", []ArrConfig{{Name: "sonarr anime"}}, true},
		{"empty name rejected", []ArrConfig{{Name: ""}}, true},
		{"leading hyphen rejected", []ArrConfig{{Name: "-sonarr"}}, true},
		{"trailing hyphen rejected", []ArrConfig{{Name: "sonarr-"}}, true},
		{"double hyphen rejected", []ArrConfig{{Name: "sonarr--anime"}}, true},
		{"duplicate name rejected", []ArrConfig{{Name: "sonarr"}, {Name: "sonarr"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArrs(tt.arrs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateArrs(%v) error = %v, wantErr %v", tt.arrs, err, tt.wantErr)
			}
		})
	}
}
