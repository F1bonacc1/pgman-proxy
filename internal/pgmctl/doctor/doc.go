// Package doctor renders the server-published doctor catalogue and
// drives the interactive --fix flow (FR-022..FR-027).
//
// The check battery and the fix registry live server-side; this
// package only renders results and routes fix prompts by blast
// radius.
package doctor
