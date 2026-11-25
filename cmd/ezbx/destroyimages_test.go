package main

import (
	"strings"
	"testing"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
)

func names(imgs []box.LocalImage) string {
	var out []string
	for _, i := range imgs {
		out = append(out, i.Name)
	}
	return strings.Join(out, " ")
}

func TestDestroyImages(t *testing.T) {
	imgs := []box.LocalImage{
		{ID: "1", Name: "ezbx-gateway:g"}, // every box's
		{ID: "2", Name: "ezbx-box:mine"},  // only this box's
		{ID: "3", Name: "ezbx-box:theirs"},
		{ID: "4", Name: "ezbx-box:old"},  // an older build, nobody's now
		{ID: "5", Name: "ezbx-box:used"}, // this box's, but a container uses it
		{ID: "6", Name: "<none>:<none>"}, // used by its ID
	}
	used := map[string]bool{"ezbx-box:used": true, "6": true}
	own := map[string]bool{"ezbx-gateway:g": true, "ezbx-box:mine": true, "ezbx-box:used": true}
	others := map[string]string{"ezbx-gateway:g": "agent-ada", "ezbx-box:theirs": "agent-ada"}

	remove, kept := destroyImages(imgs, used, own, others, false)
	if got := names(remove); got != "ezbx-box:mine" {
		t.Errorf("default removes %s", got)
	}
	k := keptText(kept)
	if strings.Join(k, "; ") != "ezbx-gateway:g: box agent-ada needs it too; ezbx-box:used: another container uses it; <none>:<none>: another container uses it" {
		t.Errorf("kept: %q", k)
	}

	remove, kept = destroyImages(imgs, used, own, others, true)
	if got := names(remove); got != "ezbx-gateway:g ezbx-box:mine ezbx-box:theirs ezbx-box:old" {
		t.Errorf("--images removes %s", got)
	}
	if len(kept) != 2 {
		t.Errorf("--images keeps %v", keptText(kept))
	}
}

func TestImageOwnersAndConfigWithout(t *testing.T) {
	cfg, err := config.Parse("version = 1\n" +
		"[box.main]\nworkspace = '/a'\nharnesses = ['pi']\ntools = []\n" +
		"[box.agent-ada]\nworkspace = '/b'\nnetwork = 'allow'\nallow = ['pypi.org']\nharnesses = ['opencode']\ntools = []\n" +
		"[box.agent-bea]\nworkspace = '/c'\nharnesses = ['pi']\ntools = []\n" +
		"[engine]\nurl = 'http://127.0.0.1:18080'\n")
	if err != nil {
		t.Fatal(err)
	}
	gw, pi, _ := imageSpecs([]string{}, []string{"pi"})
	_, oc, _ := imageSpecs([]string{}, []string{"opencode"})
	own, others := imageOwners(cfg, "main")
	if len(own) != 2 || !own[gw.Tag()] || !own[pi.Tag()] {
		t.Errorf("own: %v", own)
	}
	// The first other box by name is named, so the answer doesn't change between runs.
	if others[gw.Tag()] != "agent-ada" || others[pi.Tag()] != "agent-bea" || others[oc.Tag()] != "agent-ada" {
		t.Errorf("others: %v", others)
	}
	if own, others := imageOwners(nil, "main"); len(own) != 0 || len(others) != 0 {
		t.Error("no config, no owners")
	}

	text, ok := configWithout(cfg, "agent-ada", time.Now())
	if !ok {
		t.Fatal("other boxes stay")
	}
	back, err := config.Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if got := strings.Join(back.Names(), ","); got != "agent-bea,main" || back.Engine.URL != "http://127.0.0.1:18080" {
		t.Errorf("after removing agent-ada: %s, %+v", got, back.Engine)
	}
	one, _ := config.Parse("version = 1\n[box.main]\nworkspace = '/a'\nharnesses = ['pi']\n")
	if _, ok := configWithout(one, "main", time.Now()); ok {
		t.Error("the only box: the whole file goes")
	}
	if text, ok := configWithout(one, "other", time.Now()); !ok || !strings.Contains(text, "[box.main]") {
		t.Error("a box that isn't there: the others stay")
	}
}
