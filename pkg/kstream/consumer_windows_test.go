/*
* Copyright 2019-2020 by Nedim Sabic Sabic
* https://www.fibratus.io
* All Rights Reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*  http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */
package kstream

import (
	"context"
	"fmt"
	"github.com/rabbitstack/fibratus/pkg/config"
	"github.com/rabbitstack/fibratus/pkg/handle"
	htypes "github.com/rabbitstack/fibratus/pkg/handle/types"
	"github.com/rabbitstack/fibratus/pkg/kevent"
	"github.com/rabbitstack/fibratus/pkg/kevent/kparams"
	"github.com/rabbitstack/fibratus/pkg/kevent/ktypes"
	"github.com/rabbitstack/fibratus/pkg/ps"
	pstypes "github.com/rabbitstack/fibratus/pkg/ps/types"
	"github.com/rabbitstack/fibratus/pkg/sys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// MockListener receives the event and does nothing but indicating the event was processed.
type MockListener struct {
	gotEvent bool
}

func (l *MockListener) ProcessEvent(e *kevent.Kevent) (bool, error) {
	l.gotEvent = true
	return true, nil
}

func TestRundownEvents(t *testing.T) {
	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)

	kstreamConfig := config.KstreamConfig{
		EnableThreadKevents:   true,
		EnableImageKevents:    true,
		EnableFileIOKevents:   true,
		EnableNetKevents:      true,
		EnableRegistryKevents: true,
	}

	kctrl := NewController(kstreamConfig)
	require.NoError(t, kctrl.Start())
	defer kctrl.Close()
	kstreamc := NewConsumer(psnap, hsnap, &config.Config{
		Kstream:  kstreamConfig,
		KcapFile: "fake.kcap", // simulate capture to receive state/rundown events
		Filters:  &config.Filters{},
	})
	l := &MockListener{}
	kstreamc.RegisterEventListener(l)
	require.NoError(t, kstreamc.Open())
	defer kstreamc.Close()

	rundownsByType := map[ktypes.Ktype]bool{
		ktypes.ProcessRundown: false,
		ktypes.ThreadRundown:  false,
		ktypes.ImageRundown:   false,
		ktypes.FileRundown:    false,
		ktypes.RegKCBRundown:  false,
	}
	rundownsByHash := make(map[uint64]uint8)
	timeout := time.After(time.Second * 10)

	for {
		select {
		case e := <-kstreamc.Events():
			if !e.IsRundown() {
				continue
			}
			rundownsByType[e.Type] = true
			rundownsByHash[e.RundownKey()]++
		case err := <-kstreamc.Errors():
			t.Fatalf("FAIL: %v", err)
		case <-timeout:
			t.Logf("got %d rundown events", len(rundownsByHash))
			for key, count := range rundownsByHash {
				if count > 1 {
					t.Fatalf("got more than 1 rundown event for key %d", key)
				}
			}
			for typ, got := range rundownsByType {
				if !got {
					t.Fatalf("no rundown events for %s", typ.String())
				}
			}
			return
		}
	}
}

func TestConsumerEvents(t *testing.T) {
	kevent.DropCurrentProc = false
	var viewBase uintptr
	var freeAddress uintptr
	var dupHandleID windows.Handle

	var tests = []*struct {
		name      string
		gen       func() error
		want      func(e *kevent.Kevent) bool
		completed bool
	}{
		{
			"spawn new process",
			func() error {
				var si windows.StartupInfo
				var pi windows.ProcessInformation
				argv, err := windows.UTF16PtrFromString(filepath.Join(os.Getenv("windir"), "notepad.exe"))
				if err != nil {
					return err
				}
				err = windows.CreateProcess(
					nil,
					argv,
					nil,
					nil,
					true,
					0,
					nil,
					nil,
					&si,
					&pi)
				if err != nil {
					return err
				}
				defer windows.TerminateProcess(pi.Process, 0)
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.IsCreateProcess() && e.CurrentPid() &&
					strings.EqualFold(e.GetParamAsString(kparams.ProcessName), "notepad.exe")
			},
			false,
		},
		{
			"terminate process",
			nil,
			func(e *kevent.Kevent) bool {
				return e.IsTerminateProcess() && strings.EqualFold(e.GetParamAsString(kparams.ProcessName), "notepad.exe")
			},
			false,
		},
		{
			"load image",
			nil,
			func(e *kevent.Kevent) bool {
				img := filepath.Join(os.Getenv("windir"), "System32", "notepad.exe")
				// should get a catalog-signed binary
				signatureType := e.GetParamAsString(kparams.ImageSignatureType)
				return e.IsLoadImage() && strings.EqualFold(img, e.GetParamAsString(kparams.ImageFilename)) &&
					signatureType == "CATALOG_CACHED"
			},
			false,
		},
		{
			"create new file",
			func() error {
				f, err := os.CreateTemp(os.TempDir(), "fibratus-test")
				if err != nil {
					return err
				}
				defer f.Close()
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.CreateFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(kparams.FileName)), "fibratus-test") &&
					e.GetParamAsString(kparams.FileOperation) != "OPEN"
			},
			false,
		},
		{
			"connect socket",
			func() error {
				go func() {
					srv := http.Server{
						Addr: ":18090",
					}
					mux := http.NewServeMux()
					mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {})
					time.AfterFunc(time.Second*2, func() {
						//nolint:noctx
						resp, _ := http.Get("http://localhost:18090")
						if resp != nil {
							defer func() {
								_ = resp.Body.Close()
							}()
						}
						_ = srv.Shutdown(context.TODO())
					})
					_ = srv.ListenAndServe()
				}()
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && (e.Type == ktypes.ConnectTCPv4 || e.Type == ktypes.ConnectTCPv6)
			},
			false,
		},
		{
			"map view section",
			func() error {
				const SecImage = 0x01000000
				const SectionRead = 0x4

				var sec windows.Handle
				var offset uintptr
				var baseViewAddr uintptr
				dll := "../yara/_fixtures/yara-test.dll"
				f, err := os.Open(dll)
				if err != nil {
					return err
				}
				defer f.Close()
				stat, err := f.Stat()
				if err != nil {
					return err
				}
				size := stat.Size()
				if err := sys.NtCreateSection(
					&sec,
					SectionRead,
					0,
					uintptr(unsafe.Pointer(&size)),
					windows.PAGE_READONLY,
					SecImage,
					windows.Handle(f.Fd()),
				); err != nil {
					return fmt.Errorf("NtCreateSection: %v", err)
				}
				defer windows.Close(sec)
				err = sys.NtMapViewOfSection(
					sec,
					windows.CurrentProcess(),
					uintptr(unsafe.Pointer(&baseViewAddr)),
					0,
					0,
					uintptr(unsafe.Pointer(&offset)),
					uintptr(unsafe.Pointer(&size)),
					windows.SUB_CONTAINERS_ONLY_INHERIT,
					0,
					windows.PAGE_READONLY)
				if err != nil {
					return fmt.Errorf("NtMapViewOfSection: %v", err)
				}
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.MapViewFile &&
					e.GetParamAsString(kparams.MemProtect) == "EXECUTE_READWRITE|READONLY" &&
					e.GetParamAsString(kparams.FileViewSectionType) == "IMAGE" &&
					strings.Contains(e.GetParamAsString(kparams.FileName), "pkg\\yara\\_fixtures\\yara-test.dll")
			},
			false,
		},
		{
			"unmap view section",
			func() error {
				const SecCommit = 0x8000000
				const SectionWrite = 0x2
				const SectionRead = 0x4
				const SectionExecute = 0x8
				const SectionRWX = SectionRead | SectionWrite | SectionExecute

				var sec windows.Handle
				var size uint64 = 1024
				var offset uintptr
				if err := sys.NtCreateSection(
					&sec,
					SectionRWX,
					0,
					uintptr(unsafe.Pointer(&size)),
					windows.PAGE_READONLY,
					SecCommit,
					0,
				); err != nil {
					return fmt.Errorf("NtCreateSection: %v", err)
				}
				defer windows.Close(sec)
				err := sys.NtMapViewOfSection(
					sec,
					windows.CurrentProcess(),
					uintptr(unsafe.Pointer(&viewBase)),
					0,
					0,
					uintptr(unsafe.Pointer(&offset)),
					uintptr(unsafe.Pointer(&size)),
					windows.SUB_CONTAINERS_ONLY_INHERIT,
					0,
					windows.PAGE_READONLY)
				if err != nil {
					return fmt.Errorf("NtMapViewOfSection: %v", err)
				}
				return sys.NtUnmapViewOfSection(windows.CurrentProcess(), viewBase)
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.UnmapViewFile &&
					e.GetParamAsString(kparams.MemProtect) == "READONLY" &&
					e.Kparams.MustGetUint64(kparams.FileViewBase) == uint64(viewBase)
			},
			false,
		},
		{
			"virtual alloc",
			func() error {
				base, err := windows.VirtualAlloc(0, 1024, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_EXECUTE_READWRITE)
				if err != nil {
					return err
				}
				defer func() {
					_ = windows.VirtualFree(base, 1024, windows.MEM_RELEASE)
				}()
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.VirtualAlloc &&
					e.GetParamAsString(kparams.MemAllocType) == "COMMIT|RESERVE" && e.GetParamAsString(kparams.MemProtectMask) == "RWX"
			},
			false,
		},
		{
			"virtual free",
			func() error {
				var err error
				freeAddress, err = windows.VirtualAlloc(0, 1024, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_EXECUTE_READWRITE)
				if err != nil {
					return err
				}
				return windows.VirtualFree(freeAddress, 1024, windows.MEM_DECOMMIT)
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.VirtualFree &&
					e.GetParamAsString(kparams.MemAllocType) == "DECOMMIT" && e.Kparams.MustGetUint64(kparams.MemBaseAddress) == uint64(freeAddress)
			},
			false,
		},
		{
			"duplicate handle",
			func() error {
				var si windows.StartupInfo
				var pi windows.ProcessInformation
				argv, err := windows.UTF16PtrFromString(filepath.Join(os.Getenv("windir"), "notepad.exe"))
				if err != nil {
					return err
				}
				err = windows.CreateProcess(
					nil,
					argv,
					nil,
					nil,
					true,
					0,
					nil,
					nil,
					&si,
					&pi)
				if err != nil {
					return err
				}
				time.Sleep(time.Second)
				defer windows.TerminateProcess(pi.Process, 0)
				hs := handle.NewSnapshotter(&config.Config{}, nil)
				handles, err := hs.FindHandles(pi.ProcessId)
				if err != nil {
					return err
				}
				for _, h := range handles {
					if h.Type == handle.Key {
						dupHandleID = h.Num
						break
					}
				}
				assert.False(t, dupHandleID == 0)
				dup, err := handle.Duplicate(dupHandleID, pi.ProcessId, windows.KEY_READ)
				if err != nil {
					return err
				}
				defer windows.Close(dup)
				return nil
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.DuplicateHandle &&
					e.GetParamAsString(kparams.HandleObjectTypeID) == handle.Key &&
					windows.Handle(e.Kparams.MustGetUint32(kparams.HandleSourceID)) == dupHandleID
			},
			false,
		},
		{
			"query dns",
			func() error {
				_, err := net.LookupHost("dns.google")
				return err
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.QueryDNS && e.IsDNS() &&
					e.Type.Subcategory() == ktypes.DNS &&
					e.GetParamAsString(kparams.DNSName) == "dns.google" &&
					e.GetParamAsString(kparams.DNSRR) == "A"
			},
			false,
		},
		{
			"reply dns",
			func() error {
				_, err := net.LookupHost("dns.google")
				return err
			},
			func(e *kevent.Kevent) bool {
				return e.CurrentPid() && e.Type == ktypes.ReplyDNS && e.IsDNS() &&
					e.Type.Subcategory() == ktypes.DNS &&
					e.GetParamAsString(kparams.DNSName) == "dns.google" &&
					e.GetParamAsString(kparams.DNSRR) == "AAAA" &&
					e.GetParamAsString(kparams.DNSRcode) == "NOERROR" &&
					e.GetParamAsString(kparams.DNSAnswers) != ""
			},
			false,
		},
	}

	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)
	hsnap.On("Write", mock.Anything).Return(nil)
	hsnap.On("Remove", mock.Anything).Return(nil)

	kstreamConfig := config.KstreamConfig{
		EnableThreadKevents:   true,
		EnableImageKevents:    true,
		EnableFileIOKevents:   true,
		EnableNetKevents:      true,
		EnableRegistryKevents: true,
		EnableMemKevents:      true,
		EnableHandleKevents:   true,
		EnableDNSEvents:       true,
	}

	kctrl := NewController(kstreamConfig)
	require.NoError(t, kctrl.Start())
	defer kctrl.Close()
	kstreamc := NewConsumer(psnap, hsnap, &config.Config{Kstream: kstreamConfig, Filters: &config.Filters{}})
	l := &MockListener{}
	kstreamc.RegisterEventListener(l)
	require.NoError(t, kstreamc.Open())
	defer kstreamc.Close()

	time.Sleep(time.Second * 2)

	for _, tt := range tests {
		gen := tt.gen
		if gen != nil {
			require.NoError(t, gen(), tt.name)
		}
	}

	ntests := len(tests)
	timeout := time.After(time.Duration(ntests) * time.Minute)

	for {
		select {
		case e := <-kstreamc.Events():
			for _, tt := range tests {
				if tt.completed {
					continue
				}
				pred := tt.want
				if pred(e) {
					t.Logf("PASS: %s", tt.name)
					tt.completed = true
					ntests--
				}
				if ntests == 0 {
					assert.True(t, l.gotEvent)
					return
				}
			}
		case err := <-kstreamc.Errors():
			t.Fatalf("FAIL: %v", err)
		case <-timeout:
			for _, tt := range tests {
				if !tt.completed {
					t.Logf("FAIL: %s", tt.name)
				}
			}
			t.Fatal("FAIL: TestConsumerEvents")
		}
	}
}
