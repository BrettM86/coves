package imageproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetPreset(t *testing.T) {
	tests := []struct {
		name        string
		presetName  string
		wantWidth   int
		wantHeight  int
		wantFit     FitMode
		wantQuality int
		wantErr     error
	}{
		{
			name:        "avatar preset returns correct dimensions",
			presetName:  "avatar",
			wantWidth:   1000,
			wantHeight:  1000,
			wantFit:     FitCover,
			wantQuality: 85,
			wantErr:     nil,
		},
		{
			name:        "avatar_small preset returns correct dimensions",
			presetName:  "avatar_small",
			wantWidth:   360,
			wantHeight:  360,
			wantFit:     FitCover,
			wantQuality: 80,
			wantErr:     nil,
		},
		{
			name:        "banner preset returns correct dimensions",
			presetName:  "banner",
			wantWidth:   640,
			wantHeight:  300,
			wantFit:     FitCover,
			wantQuality: 85,
			wantErr:     nil,
		},
		{
			name:        "content_preview preset returns correct dimensions",
			presetName:  "content_preview",
			wantWidth:   800,
			wantHeight:  0,
			wantFit:     FitContain,
			wantQuality: 80,
			wantErr:     nil,
		},
		{
			name:        "content_full preset returns correct dimensions",
			presetName:  "content_full",
			wantWidth:   1600,
			wantHeight:  0,
			wantFit:     FitContain,
			wantQuality: 90,
			wantErr:     nil,
		},
		{
			name:        "embed_thumbnail preset returns correct dimensions",
			presetName:  "embed_thumbnail",
			wantWidth:   720,
			wantHeight:  360,
			wantFit:     FitCover,
			wantQuality: 80,
			wantErr:     nil,
		},
		{
			name:       "invalid preset returns error",
			presetName: "invalid",
			wantErr:    ErrInvalidPreset,
		},
		{
			name:       "empty preset name returns error",
			presetName: "",
			wantErr:    ErrInvalidPreset,
		},
		{
			name:       "case sensitive - AVATAR should not match",
			presetName: "AVATAR",
			wantErr:    ErrInvalidPreset,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preset, err := GetPreset(tt.presetName)

			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.presetName, preset.Name)
			assert.Equal(t, tt.wantWidth, preset.Width)
			assert.Equal(t, tt.wantHeight, preset.Height)
			assert.Equal(t, tt.wantFit, preset.Fit)
			assert.Equal(t, tt.wantQuality, preset.Quality)
		})
	}
}

func TestAllPresetsHaveValidDimensions(t *testing.T) {
	presetNames := []string{
		"avatar",
		"avatar_small",
		"banner",
		"content_preview",
		"content_full",
		"embed_thumbnail",
	}

	for _, name := range presetNames {
		t.Run(name, func(t *testing.T) {
			preset, err := GetPreset(name)
			require.NoError(t, err)

			// Width must always be positive
			assert.Greater(t, preset.Width, 0, "preset %s must have positive width", name)

			// Height can be 0 for contain fit (proportional scaling)
			if preset.Fit == FitCover {
				assert.Greater(t, preset.Height, 0, "cover fit preset %s must have positive height", name)
			}

			// Quality must be between 1 and 100
			assert.GreaterOrEqual(t, preset.Quality, 1, "preset %s quality must be >= 1", name)
			assert.LessOrEqual(t, preset.Quality, 100, "preset %s quality must be <= 100", name)

			// Name must match
			assert.Equal(t, name, preset.Name)
		})
	}
}

func TestFitModeString(t *testing.T) {
	tests := []struct {
		mode FitMode
		want string
	}{
		{FitCover, "cover"},
		{FitContain, "contain"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.mode.String())
		})
	}
}

func TestListPresets(t *testing.T) {
	presets := ListPresets()

	// Should have all 6 presets
	assert.Len(t, presets, 6)

	// Verify all expected presets are present
	expectedNames := map[string]bool{
		"avatar":          false,
		"avatar_small":    false,
		"banner":          false,
		"content_preview": false,
		"content_full":    false,
		"embed_thumbnail": false,
	}

	for _, p := range presets {
		if _, exists := expectedNames[p.Name]; exists {
			expectedNames[p.Name] = true
		}
	}

	for name, found := range expectedNames {
		assert.True(t, found, "expected preset %s to be in list", name)
	}
}

func TestPreset_Validate(t *testing.T) {
	tests := []struct {
		name    string
		preset  Preset
		wantErr bool
	}{
		{
			name:    "valid avatar preset",
			preset:  Preset{Name: "avatar", Width: 1000, Height: 1000, Fit: FitCover, Quality: 85},
			wantErr: false,
		},
		{
			name:    "valid contain preset with zero height",
			preset:  Preset{Name: "content", Width: 800, Height: 0, Fit: FitContain, Quality: 80},
			wantErr: false,
		},
		{
			name:    "invalid empty name",
			preset:  Preset{Name: "", Width: 160, Height: 160, Fit: FitCover, Quality: 85},
			wantErr: true,
		},
		{
			name:    "invalid zero width",
			preset:  Preset{Name: "test", Width: 0, Height: 160, Fit: FitCover, Quality: 85},
			wantErr: true,
		},
		{
			name:    "invalid negative width",
			preset:  Preset{Name: "test", Width: -100, Height: 160, Fit: FitCover, Quality: 85},
			wantErr: true,
		},
		{
			name:    "invalid cover fit with zero height",
			preset:  Preset{Name: "test", Width: 160, Height: 0, Fit: FitCover, Quality: 85},
			wantErr: true,
		},
		{
			name:    "invalid quality zero",
			preset:  Preset{Name: "test", Width: 160, Height: 160, Fit: FitCover, Quality: 0},
			wantErr: true,
		},
		{
			name:    "invalid quality over 100",
			preset:  Preset{Name: "test", Width: 160, Height: 160, Fit: FitCover, Quality: 101},
			wantErr: true,
		},
		{
			name:    "valid quality at boundary 1",
			preset:  Preset{Name: "test", Width: 160, Height: 160, Fit: FitCover, Quality: 1},
			wantErr: false,
		},
		{
			name:    "valid quality at boundary 100",
			preset:  Preset{Name: "test", Width: 160, Height: 160, Fit: FitCover, Quality: 100},
			wantErr: false,
		},
		{
			name:    "invalid unknown fit mode",
			preset:  Preset{Name: "test", Width: 160, Height: 160, Fit: FitMode("unknown"), Quality: 85},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.preset.Validate()
			if tt.wantErr {
				assert.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidPreset)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAllPresetsValidate(t *testing.T) {
	// All built-in presets should pass validation
	for _, preset := range ListPresets() {
		t.Run(preset.Name, func(t *testing.T) {
			err := preset.Validate()
			assert.NoError(t, err, "built-in preset %s should be valid", preset.Name)
		})
	}
}
