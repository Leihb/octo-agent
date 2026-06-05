// Package themes provides compact glamour markdown styles for the TUI.
// They are derived from glamour's default dark/light themes with reduced
// vertical margins (document.margin=0, code_block.margin=0, heading.block_suffix="").
package themes

import (
	_ "embed"
)

//go:embed octo-dark.json
var darkStyle []byte

//go:embed octo-light.json
var lightStyle []byte

// GetStyle returns the glamour style JSON for the given theme name.
// Supported values are "dark" and "light"; any other value falls back to dark.
func GetStyle(name string) []byte {
	if name == "light" {
		return lightStyle
	}
	return darkStyle
}
