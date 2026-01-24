package imageproxy

// FitMode defines how an image should be fitted to the target dimensions.
type FitMode string

const (
	// FitCover scales the image to cover the target dimensions, cropping if necessary.
	FitCover FitMode = "cover"
	// FitContain scales the image to fit within the target dimensions, preserving aspect ratio.
	FitContain FitMode = "contain"
)

// String returns the string representation of the FitMode.
func (f FitMode) String() string {
	return string(f)
}

// Preset defines the configuration for an image transformation preset.
type Preset struct {
	Name    string
	Width   int
	Height  int
	Fit     FitMode
	Quality int
}

// Validate checks that the preset has valid configuration values.
// Returns nil if valid, or an error describing what is wrong.
func (p Preset) Validate() error {
	if p.Name == "" {
		return ErrInvalidPreset
	}
	if p.Width <= 0 {
		return ErrInvalidPreset
	}
	// Height can be 0 for FitContain (proportional scaling)
	if p.Fit == FitCover && p.Height <= 0 {
		return ErrInvalidPreset
	}
	// Quality must be in JPEG range (1-100)
	if p.Quality < 1 || p.Quality > 100 {
		return ErrInvalidPreset
	}
	// Validate FitMode is a known value
	if p.Fit != FitCover && p.Fit != FitContain {
		return ErrInvalidPreset
	}
	return nil
}

// presets is the registry of all available image presets.
var presets = map[string]Preset{
	"avatar": {
		Name:    "avatar",
		Width:   1000,
		Height:  1000,
		Fit:     FitCover,
		Quality: 85,
	},
	"avatar_small": {
		Name:    "avatar_small",
		Width:   360,
		Height:  360,
		Fit:     FitCover,
		Quality: 80,
	},
	"banner": {
		Name:    "banner",
		Width:   640,
		Height:  300,
		Fit:     FitCover,
		Quality: 85,
	},
	"content_preview": {
		Name:    "content_preview",
		Width:   800,
		Height:  0,
		Fit:     FitContain,
		Quality: 80,
	},
	"content_full": {
		Name:    "content_full",
		Width:   1600,
		Height:  0,
		Fit:     FitContain,
		Quality: 90,
	},
	"embed_thumbnail": {
		Name:    "embed_thumbnail",
		Width:   720,
		Height:  360,
		Fit:     FitCover,
		Quality: 80,
	},
}

// GetPreset returns the preset configuration for the given name.
// Returns ErrInvalidPreset if the preset name is not found.
func GetPreset(name string) (Preset, error) {
	if name == "" {
		return Preset{}, ErrInvalidPreset
	}
	preset, exists := presets[name]
	if !exists {
		return Preset{}, ErrInvalidPreset
	}
	return preset, nil
}

// ListPresets returns all available presets.
func ListPresets() []Preset {
	result := make([]Preset, 0, len(presets))
	for _, p := range presets {
		result = append(result, p)
	}
	return result
}
