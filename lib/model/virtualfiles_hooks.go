// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"log/slog"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
)

// VirtualFileEventType describes internal lifecycle events that a future
// desktop virtual filesystem bridge can subscribe to.
type VirtualFileEventType string

const (
	VirtualFileEventMetadataOnlySet VirtualFileEventType = "metadataOnlySet"
	VirtualFileEventPresenceChanged VirtualFileEventType = "presenceChanged"
	VirtualFileEventFetchStarted    VirtualFileEventType = "fetchStarted"
	VirtualFileEventFetchCompleted  VirtualFileEventType = "fetchCompleted"
	VirtualFileEventFetchFailed     VirtualFileEventType = "fetchFailed"
)

// VirtualFileEvent carries experimental virtual-file state changes to
// integration hooks. Phase 4 adds the hook seam but does not expose any
// platform-specific filesystem implementation yet.
type VirtualFileEvent struct {
	Type    VirtualFileEventType `json:"type"`
	Folder  string               `json:"folder"`
	File    string               `json:"file"`
	State   PresenceState        `json:"state"`
	Backend string               `json:"backend"`
	Error   string               `json:"error,omitempty"`
	Time    time.Time            `json:"time"`
}

// VirtualFilesystemHook receives internal virtual-file lifecycle events.
type VirtualFilesystemHook interface {
	HandleVirtualFileEvent(VirtualFileEvent)
}

// RegisterVirtualFilesystemHook installs an internal hook for future desktop
// virtual filesystem integrations. The returned function unregisters the hook.
func (m *model) RegisterVirtualFilesystemHook(hook VirtualFilesystemHook) func() {
	m.mut.Lock()
	id := m.nextVirtualFileHookID
	m.nextVirtualFileHookID++
	m.virtualFileHooks[id] = hook
	m.mut.Unlock()

	return func() {
		m.mut.Lock()
		delete(m.virtualFileHooks, id)
		m.mut.Unlock()
	}
}

func (m *model) newVirtualFileEvent(eventType VirtualFileEventType, presence FilePresence, err error) VirtualFileEvent {
	event := VirtualFileEvent{
		Type:    eventType,
		Folder:  presence.Folder,
		File:    presence.File,
		State:   presence.State,
		Backend: presence.Backend,
		Time:    time.Now().UTC(),
	}
	if err != nil {
		event.Error = err.Error()
	}
	return event
}

func (m *model) notifyVirtualFileHooks(event VirtualFileEvent) {
	m.mut.RLock()
	hooks := make([]VirtualFilesystemHook, 0, len(m.virtualFileHooks))
	for _, hook := range m.virtualFileHooks {
		hooks = append(hooks, hook)
	}
	m.mut.RUnlock()

	// Delivery remains synchronous to preserve deterministic ordering with the
	// state transition that triggered the event. These hooks are internal and
	// experimental; panic recovery isolates bad hooks without introducing a
	// background queue that could reorder or drop lifecycle events.
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		func(hook VirtualFilesystemHook) {
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Error("Virtual-file hook panicked", slog.String("folder", event.Folder), slogutil.FilePath(event.File), slog.String("type", string(event.Type)), slog.Any("panic", recovered))
				}
			}()
			hook.HandleVirtualFileEvent(event)
		}(hook)
	}
}
