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

package processors

import (
	"expvar"
	"github.com/rabbitstack/fibratus/pkg/config"
	"github.com/rabbitstack/fibratus/pkg/fs"
	"github.com/rabbitstack/fibratus/pkg/handle"
	htypes "github.com/rabbitstack/fibratus/pkg/handle/types"
	"github.com/rabbitstack/fibratus/pkg/kevent"
	"github.com/rabbitstack/fibratus/pkg/kevent/kparams"
	"github.com/rabbitstack/fibratus/pkg/kevent/ktypes"
	"github.com/rabbitstack/fibratus/pkg/ps"
	"github.com/rabbitstack/fibratus/pkg/sys"
	"github.com/rabbitstack/fibratus/pkg/util/va"
	"golang.org/x/sys/windows"
	"golang.org/x/time/rate"
	"sync"
	"time"
)

var (
	// totalRundownFiles counts the number of opened files
	totalRundownFiles    = expvar.NewInt("fs.total.rundown.files")
	totalMapRundownFiles = expvar.NewInt("fs.total.map.rundown.files")
	// fileObjectMisses computes file object cache misses
	fileObjectMisses     = expvar.NewInt("fs.file.objects.misses")
	fileObjectHandleHits = expvar.NewInt("fs.file.object.handle.hits")
	fileReleaseCount     = expvar.NewInt("fs.file.releases")

	fsFileCharacteristicsRateLimits = expvar.NewInt("fs.file.characteristics.rate.limits")
)

type fsProcessor struct {
	// files stores the file metadata indexed by file object
	files map[uint64]*FileInfo

	hsnap handle.Snapshotter
	psnap ps.Snapshotter

	// irps contains a mapping between the IRP (I/O request packet) and CreateFile events
	irps map[uint64]*kevent.Kevent

	devMapper       fs.DevMapper
	devPathResolver fs.DevPathResolver
	config          *config.Config

	// buckets stores stack walk events per stack id
	buckets map[uint64][]*kevent.Kevent
	mu      sync.Mutex
	purger  *time.Ticker

	quit chan struct{}
	// lim throttles the parsing of image characteristics
	lim *rate.Limiter
}

// FileInfo stores file information obtained from event state.
type FileInfo struct {
	Name string
	Type fs.FileType
}

func newFsProcessor(
	hsnap handle.Snapshotter,
	psnap ps.Snapshotter,
	devMapper fs.DevMapper,
	devPathResolver fs.DevPathResolver,
	config *config.Config,
) Processor {
	f := &fsProcessor{
		files:           make(map[uint64]*FileInfo),
		irps:            make(map[uint64]*kevent.Kevent),
		hsnap:           hsnap,
		psnap:           psnap,
		devMapper:       devMapper,
		devPathResolver: devPathResolver,
		config:          config,
		buckets:         make(map[uint64][]*kevent.Kevent),
		purger:          time.NewTicker(time.Second * 5),
		quit:            make(chan struct{}, 1),
		lim:             rate.NewLimiter(30, 40), // allow 30 parse ops per second or bursts of 40 ops
	}

	go f.purge()

	return f
}

func (f *fsProcessor) ProcessEvent(e *kevent.Kevent) (*kevent.Kevent, bool, error) {
	if e.Category == ktypes.File || e.IsStackWalk() {
		evt, err := f.processEvent(e)
		return evt, false, err
	}
	return e, true, nil
}

func (*fsProcessor) Name() ProcessorType { return Fs }
func (f *fsProcessor) Close()            { f.quit <- struct{}{} }

func (f *fsProcessor) getFileInfo(name string, opts uint32) *FileInfo {
	return &FileInfo{Name: name, Type: fs.GetFileType(name, opts)}
}

func (f *fsProcessor) processEvent(e *kevent.Kevent) (*kevent.Kevent, error) {
	switch e.Type {
	case ktypes.FileRundown:
		// when the file rundown event comes in we store the file info
		// in internal state in order to augment the rest of file events
		// that lack the file path field
		filepath := e.GetParamAsString(kparams.FilePath)
		fileObject, err := e.Kparams.GetUint64(kparams.FileObject)
		if err != nil {
			return nil, err
		}
		if _, ok := f.files[fileObject]; !ok {
			totalRundownFiles.Add(1)
			f.files[fileObject] = &FileInfo{Name: filepath, Type: fs.GetFileType(filepath, 0)}
		}
	case ktypes.MapFileRundown:
		fileKey := e.Kparams.MustGetUint64(kparams.FileKey)
		fileinfo := f.files[fileKey]

		if fileinfo != nil {
			totalMapRundownFiles.Add(1)
			e.AppendParam(kparams.FilePath, kparams.Path, fileinfo.Name)
		} else {
			// if the view of section is backed by the data/image file
			// try to get the mapped file name and append it to params
			sec := e.Kparams.MustGetUint32(kparams.FileViewSectionType)
			isMapped := sec != va.SectionPagefile && sec != va.SectionPhysical
			if isMapped {
				totalMapRundownFiles.Add(1)
				addr := e.Kparams.MustGetUint64(kparams.FileViewBase) + (e.Kparams.MustGetUint64(kparams.FileOffset))
				e.AppendParam(kparams.FilePath, kparams.Path, f.getMappedFile(e.PID, addr))
			}
		}

		return e, f.psnap.AddMmap(e)
	case ktypes.CreateFile:
		// we defer the processing of the CreateFile event until we get
		// the matching FileOpEnd event. This event contains the operation
		// that was done on behalf of the file, e.g. create or open.
		irp := e.Kparams.MustGetUint64(kparams.FileIrpPtr)
		e.WaitEnqueue = true
		f.irps[irp] = e
	case ktypes.StackWalk:
		if !kevent.IsCurrentProcDropped(e.PID) {
			f.mu.Lock()
			defer f.mu.Unlock()

			// append the event to the bucket indexed by stack id
			id := e.StackID()
			q, ok := f.buckets[id]
			if !ok {
				f.buckets[id] = []*kevent.Kevent{e}
			} else {
				f.buckets[id] = append(q, e)
			}
		}
	case ktypes.FileOpEnd:
		// get the CreateFile pending event by IRP identifier
		// and fetch the file create disposition value
		var (
			irp    = e.Kparams.MustGetUint64(kparams.FileIrpPtr)
			dispo  = e.Kparams.MustGetUint64(kparams.FileExtraInfo)
			status = e.Kparams.MustGetUint32(kparams.NTStatus)
		)

		if dispo > windows.FILE_MAXIMUM_DISPOSITION {
			return e, nil
		}
		ev, ok := f.irps[irp]
		if !ok {
			return e, nil
		}
		delete(f.irps, irp)

		// reset the wait status to allow passage of this event to
		// the aggregator queue. Additionally, append params to it
		ev.WaitEnqueue = false
		fileObject := ev.Kparams.MustGetUint64(kparams.FileObject)

		// try to get extended file info. If the file object is already
		// present in the map, we'll reuse the existing file information
		fileinfo, ok := f.files[fileObject]
		if !ok {
			opts := ev.Kparams.MustGetUint32(kparams.FileCreateOptions)
			opts &= 0xFFFFFF
			filepath := ev.GetParamAsString(kparams.FilePath)
			fileinfo = f.getFileInfo(filepath, opts)
			f.files[fileObject] = fileinfo
		}

		if f.config.Kstream.EnableHandleKevents {
			f.devPathResolver.AddPath(ev.GetParamAsString(kparams.FilePath))
		}

		ev.AppendParam(kparams.NTStatus, kparams.Status, status)
		if fileinfo.Type != fs.Unknown {
			ev.AppendEnum(kparams.FileType, uint32(fileinfo.Type), fs.FileTypes)
		}
		ev.AppendEnum(kparams.FileOperation, uint32(dispo), fs.FileCreateDispositions)

		// attach stack walk return addresses. CreateFile events
		// represent an edge case in callstack enrichment. Since
		// the events are delayed until the respective FileOpEnd
		// event arrives, we enable stack tracing for CreateFile
		// events. When the CreateFile event is generated, we store
		// it in pending IRP map. Subsequently, the stack walk event
		// is put inside the queue. After FileOpEnd event arrives,
		// the previous stack walk for CreateFile is popped from
		// the queue and the callstack parameter attached to the
		// event.
		if f.config.Kstream.StackEnrichment {
			f.mu.Lock()
			defer f.mu.Unlock()

			id := ev.StackID()
			q, ok := f.buckets[id]
			if ok && len(q) > 0 {
				var s *kevent.Kevent
				s, f.buckets[id] = q[len(q)-1], q[:len(q)-1]
				callstack := s.Kparams.MustGetSlice(kparams.Callstack)
				ev.AppendParam(kparams.Callstack, kparams.Slice, callstack)
			}
		}

		// parse PE data for created files and append parameters
		if ev.IsCreateDisposition() && ev.IsSuccess() {
			if !f.lim.Allow() {
				fsFileCharacteristicsRateLimits.Add(1)
				return ev, nil
			}
			path := ev.GetParamAsString(kparams.FilePath)
			c, err := parseImageFileCharacteristics(path)
			if err != nil {
				return ev, nil
			}
			ev.AppendParam(kparams.FileIsDLL, kparams.Bool, c.isDLL)
			ev.AppendParam(kparams.FileIsDriver, kparams.Bool, c.isDriver)
			ev.AppendParam(kparams.FileIsExecutable, kparams.Bool, c.isExe)
			ev.AppendParam(kparams.FileIsDotnet, kparams.Bool, c.isDotnet)
		}

		return ev, nil
	case ktypes.ReleaseFile:
		fileReleaseCount.Add(1)
		// delete file metadata by file object address
		fileObject := e.Kparams.MustGetUint64(kparams.FileObject)
		delete(f.files, fileObject)
	case ktypes.UnmapViewFile:
		ok, proc := f.psnap.Find(e.PID)
		addr := e.Kparams.TryGetAddress(kparams.FileViewBase)
		if ok {
			mmap := proc.FindMmap(addr)
			if mmap != nil {
				e.AppendParam(kparams.FilePath, kparams.Path, mmap.File)
			}
		}

		totalMapRundownFiles.Add(-1)

		return e, f.psnap.RemoveMmap(e.PID, addr)
	default:
		var fileObject uint64
		fileKey := e.Kparams.MustGetUint64(kparams.FileKey)

		if !e.IsMapViewFile() {
			fileObject = e.Kparams.MustGetUint64(kparams.FileObject)
		}

		// attempt to get the file by file key. If there is no such file referenced
		// by the file key, then try to fetch it by file object. Even if file object
		// references fails, we search in the file handles for such file
		fileinfo := f.findFile(fileKey, fileObject)

		// try to resolve mapped file name if not found in internal state
		if fileinfo == nil && e.IsMapViewFile() {
			sec := e.Kparams.MustGetUint32(kparams.FileViewSectionType)
			isMapped := sec != va.SectionPagefile && sec != va.SectionPhysical
			if isMapped {
				totalMapRundownFiles.Add(1)
				addr := e.Kparams.MustGetUint64(kparams.FileViewBase) + (e.Kparams.MustGetUint64(kparams.FileOffset))
				e.AppendParam(kparams.FilePath, kparams.Path, f.getMappedFile(e.PID, addr))
			}
		}

		// ignore object misses that are produced by CloseFile
		if fileinfo == nil && !e.IsCloseFile() {
			fileObjectMisses.Add(1)
		}

		if e.IsDeleteFile() {
			delete(f.files, fileObject)
		}
		if e.IsEnumDirectory() {
			if fileinfo != nil {
				e.AppendParam(kparams.FileDirectory, kparams.Path, fileinfo.Name)
			}
			return e, nil
		}

		if fileinfo != nil {
			if fileinfo.Type != fs.Unknown {
				e.AppendEnum(kparams.FileType, uint32(fileinfo.Type), fs.FileTypes)
			}
			e.AppendParam(kparams.FilePath, kparams.Path, fileinfo.Name)
		}

		if e.IsMapViewFile() {
			return e, f.psnap.AddMmap(e)
		}
	}

	return e, nil
}

func (f *fsProcessor) findFile(fileKey, fileObject uint64) *FileInfo {
	fileinfo, ok := f.files[fileKey]
	if ok {
		return fileinfo
	}
	fileinfo, ok = f.files[fileObject]
	if ok {
		return fileinfo
	}
	// look in the system handles for file objects
	var file htypes.Handle
	file, ok = f.hsnap.FindByObject(fileObject)
	if !ok {
		return nil
	}
	if file.Type == handle.File {
		fileObjectHandleHits.Add(1)
		return &FileInfo{Name: file.Name, Type: fs.GetFileType(file.Name, 0)}
	}
	return nil
}

func (f *fsProcessor) getMappedFile(pid uint32, addr uint64) string {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.Close(process)
	return f.devMapper.Convert(sys.GetMappedFile(process, uintptr(addr)))
}

func (f *fsProcessor) purge() {
	for {
		select {
		case <-f.purger.C:
			f.mu.Lock()

			// evict unmatched stack traces
			for id, q := range f.buckets {
				for i, evt := range q {
					if time.Since(evt.Timestamp) > time.Second*30 {
						f.buckets[id] = append(q[:i], q[i+1:]...)
					}
				}
			}

			f.mu.Unlock()
		case <-f.quit:
			return
		}
	}
}
