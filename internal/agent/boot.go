package agent

import "errors"

// bootloader boots a staged test kernel exactly once, then the Mac's default.
type bootloader interface {
	name() string
	problems() []string // what stops the one-shot from working, without changing anything
	arm(entry string) error
	armed() string
	disarm() error
	writeEntries() error
}

// detectBootloader finds the loader that boots this Mac: Limine when the
// ESP's EFI loader is Limine (Omarchy on Apple Silicon since 2026-09),
// otherwise GRUB.
func detectBootloader() (bootloader, error) {
	l, err := detectLimine()
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, errNoLimine) {
		return nil, err
	}
	g, err := detectGrub()
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (g *grubLayout) name() string       { return "grub" }
func (g *grubLayout) problems() []string { return g.hookProblems() }
