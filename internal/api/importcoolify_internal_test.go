package api

import "testing"

func TestDeclareComposeVolumes(t *testing.T) {
	for in, want := range map[string]string{
		// Coolify's raw file: the volume is used, not declared.
		"services:\n  kuma:\n    image: x\n    volumes:\n      - 'kuma-data:/app/data'\n      - ./conf:/conf\n      - /srv/x:/x\n": "services:\n  kuma:\n    image: x\n    volumes:\n      - 'kuma-data:/app/data'\n      - ./conf:/conf\n      - /srv/x:/x\nvolumes:\n  kuma-data: {}\n",
		// Declared already, one missing (long syntax).
		"services:\n  a:\n    volumes:\n      - db:/var/lib/db\n      - type: volume\n        source: cache\n        target: /cache\nvolumes:\n  db:\n    driver: local\nnetworks:\n  x: {}\n": "services:\n  a:\n    volumes:\n      - db:/var/lib/db\n      - type: volume\n        source: cache\n        target: /cache\nvolumes:\n  cache: {}\n  db:\n    driver: local\nnetworks:\n  x: {}\n",
		// Nothing to add.
		"services:\n  web:\n    image: nginx\n": "services:\n  web:\n    image: nginx\n",
	} {
		if got := declareComposeVolumes(in); got != want {
			t.Errorf("declareComposeVolumes(%q):\n%s\nwant\n%s", in, got, want)
		}
	}
}
