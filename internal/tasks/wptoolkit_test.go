package tasks

import (
	"testing"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

func TestAutoUpdates(t *testing.T) {
	info := agentproto.WordPressInfo{Installed: true, Version: "6.7.1", CoreUpdate: "6.8", CoreMinor: "6.7.2",
		Plugins: []agentproto.WordPressItem{{Name: "akismet", Update: "5.4"}}, Themes: []agentproto.WordPressItem{{Name: "twentyfive"}}}
	for _, c := range []struct {
		rules store.WordPressAuto
		info  agentproto.WordPressInfo
		want  WordPressUpdate
		ok    bool
	}{
		{store.WordPressAuto{Core: "off"}, info, WordPressUpdate{}, false},
		{store.WordPressAuto{Core: "minor"}, info, WordPressUpdate{Core: "minor"}, true},
		{store.WordPressAuto{Core: "all", Plugins: true, Themes: true}, info, WordPressUpdate{Core: "latest", Plugins: []string{"*"}}, true},
		{store.WordPressAuto{Core: "all"}, agentproto.WordPressInfo{Installed: true, Version: "6.8"}, WordPressUpdate{}, false},
		{store.WordPressAuto{Core: "minor", Plugins: true}, agentproto.WordPressInfo{Installed: true, Error: "broken"}, WordPressUpdate{}, false},
	} {
		got, ok := autoUpdates(c.rules, c.info)
		if ok != c.ok || got.Core != c.want.Core || len(got.Plugins) != len(c.want.Plugins) || len(got.Themes) != len(c.want.Themes) || (ok && !got.Auto) {
			t.Errorf("%+v: got %+v %v, want %+v %v", c.rules, got, ok, c.want, c.ok)
		}
	}
}
