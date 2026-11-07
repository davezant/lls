// Package langs embeds the built-in language specifications into the lls
// binary so that a single static executable is all that has to be copied to
// a machine (e.g. a Raspberry Pi). Users can override or add languages by
// dropping JSON files into $LLS_HOME/langs.
package langs

import "embed"

// FS holds every *.json language specification of this directory.
//
//go:embed *.json
var FS embed.FS
