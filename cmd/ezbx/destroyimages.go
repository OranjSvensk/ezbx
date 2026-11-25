package main

import (
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/setup"
)

// keptImage is an ezbx image destroy leaves, and why.
type keptImage struct{ Name, Why string }

// destroyImages chooses which of ezbx's images ezbx destroy removes. By
// default it removes only the images this box's ezbx.toml section needs
// (own), and of those not one another box in ezbx.toml needs too (others:
// image name -> box) or another container uses (used). With all (--images)
// it removes every ezbx image no other container uses, as ezbx did before
// 0.4. Images that are neither this box's nor in use are left without a
// reason in the default mode: they belong to other boxes or older builds,
// which ezbx build --prune clears.
func destroyImages(imgs []box.LocalImage, used map[string]bool, own map[string]bool, others map[string]string, all bool) (remove []box.LocalImage, kept []keptImage) {
	for _, img := range imgs {
		switch {
		case used[img.Name] || used[img.ID]:
			kept = append(kept, keptImage{img.Name, "another container uses it"})
		case all:
			remove = append(remove, img)
		case !own[img.Name]:
			// not this box's
		case others[img.Name] != "":
			kept = append(kept, keptImage{img.Name, "box " + others[img.Name] + " needs it too"})
		default:
			remove = append(remove, img)
		}
	}
	return remove, kept
}

// boxImages lists the image names each box in the config needs.
func boxImages(cfg *config.Config) map[string][]string {
	out := map[string][]string{}
	if cfg == nil {
		return out
	}
	for _, n := range cfg.Names() {
		b, _ := cfg.Get(n)
		gw, bx, err := imageSpecs(b.Tools, b.Harnesses)
		if err == nil {
			out[n] = []string{gw.Tag(), bx.Tag()}
		}
	}
	return out
}

// imageOwners splits boxImages into this box's images and the other boxes'.
func imageOwners(cfg *config.Config, name string) (own map[string]bool, others map[string]string) {
	own, others = map[string]bool{}, map[string]string{}
	for n, tags := range boxImages(cfg) {
		for _, tag := range tags {
			if n == name {
				own[tag] = true
			} else if others[tag] == "" || n < others[tag] {
				others[tag] = n
			}
		}
	}
	return own, others
}

// configWithout is ezbx.toml without one box, for ezbx destroy
// --remove-config when other boxes stay. ok is false when no other box
// stays, and the whole file goes.
func configWithout(cfg *config.Config, name string, now time.Time) (text string, ok bool) {
	for _, n := range cfg.Names() {
		if n == name {
			continue
		}
		a, _ := setup.FromConfigBox(cfg, n)
		delete(a.Others, name)
		return setup.ConfigText(a, now), true
	}
	return "", false
}
