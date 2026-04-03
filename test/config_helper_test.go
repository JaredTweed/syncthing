// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build integration
// +build integration

package integration

import (
	"os"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

func loadConfigCopy(t *testing.T, path string, myID protocol.DeviceID) config.Configuration {
	t.Helper()
	wrapper, _, err := config.Load(path, myID, events.NoopLogger)
	if err != nil {
		t.Fatal(err)
	}
	return wrapper.RawCopy()
}

func writeConfigXML(t *testing.T, path string, cfg config.Configuration) {
	t.Helper()
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := cfg.WriteXML(fd); err != nil {
		t.Fatal(err)
	}
}
