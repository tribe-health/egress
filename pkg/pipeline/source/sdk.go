// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package source

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v3"
	"github.com/tinyzimmer/go-gst/gst"
	"github.com/tinyzimmer/go-gst/gst/app"
	"go.uber.org/atomic"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/pipeline/source/sdk"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
	lksdk "github.com/livekit/server-sdk-go"
	"github.com/livekit/server-sdk-go/pkg/synchronizer"
)

const (
	subscriptionTimeout = time.Second * 30
)

type SDKSource struct {
	*config.PipelineConfig

	room *lksdk.Room
	sync *synchronizer.Synchronizer

	mu                   sync.RWMutex
	initialized          core.Fuse
	filenameReplacements map[string]string
	errors               chan error

	active      atomic.Int32
	audioWriter *sdk.AppWriter
	videoWriter *sdk.AppWriter

	startRecording chan struct{}
	endRecording   chan struct{}
}

func NewSDKSource(ctx context.Context, p *config.PipelineConfig) (*SDKSource, error) {
	ctx, span := tracer.Start(ctx, "SDKInput.New")
	defer span.End()

	startRecording := make(chan struct{})
	s := &SDKSource{
		PipelineConfig: p,
		sync: synchronizer.NewSynchronizer(func() {
			close(startRecording)
		}),
		initialized:          core.NewFuse(),
		filenameReplacements: make(map[string]string),
		startRecording:       startRecording,
		endRecording:         make(chan struct{}),
	}

	if err := s.joinRoom(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *SDKSource) StartRecording() chan struct{} {
	return s.startRecording
}

func (s *SDKSource) GetStartTime() int64 {
	return s.sync.GetStartedAt()
}

func (s *SDKSource) Playing(trackID string) {
	if w := s.getWriterForTrack(trackID); w != nil {
		w.Play()
	}
}

func (s *SDKSource) EndRecording() chan struct{} {
	return s.endRecording
}

func (s *SDKSource) GetEndTime() int64 {
	return s.sync.GetEndedAt()
}

func (s *SDKSource) CloseWriters() {
	s.sync.End()

	var wg sync.WaitGroup
	if s.audioWriter != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.audioWriter.Drain(false)
		}()
	}
	if s.videoWriter != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.videoWriter.Drain(false)
		}()
	}
	wg.Wait()
}

func (s *SDKSource) StreamStopped(trackID string) {
	s.onTrackFinished(trackID)
}

func (s *SDKSource) Close() {
	s.room.Disconnect()
}

// ----- Subscriptions -----

func (s *SDKSource) joinRoom() error {
	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed:   s.onTrackSubscribed,
			OnTrackMuted:        s.onTrackMuted,
			OnTrackUnmuted:      s.onTrackUnmuted,
			OnTrackUnsubscribed: s.onTrackUnsubscribed,
		},
		OnDisconnected: s.onDisconnected,
	}

	s.room = lksdk.CreateRoom(cb)
	logger.Debugw("connecting to room")
	if err := s.room.JoinWithToken(s.WsUrl, s.Token, lksdk.WithAutoSubscribe(false)); err != nil {
		return err
	}

	var fileIdentifier string
	tracks := make(map[string]struct{})

	switch s.RequestType {
	case types.RequestTypeTrackComposite:
		fileIdentifier = s.Info.RoomName
		if s.AudioEnabled {
			tracks[s.AudioTrackID] = struct{}{}
		}
		if s.VideoEnabled {
			tracks[s.VideoTrackID] = struct{}{}
		}

	case types.RequestTypeTrack:
		fileIdentifier = s.TrackID
		tracks[s.TrackID] = struct{}{}
	}

	numTracks := len(tracks)
	s.errors = make(chan error, numTracks)
	if err := s.subscribeToTracks(tracks); err != nil {
		return err
	}

	for i := 0; i < numTracks; i++ {
		if err := <-s.errors; err != nil {
			return err
		}
	}

	s.initialized.Break()

	if err := s.UpdateInfoFromSDK(fileIdentifier, s.filenameReplacements); err != nil {
		logger.Errorw("could not update file params", err)
		return err
	}

	return nil
}

func (s *SDKSource) subscribeToTracks(expecting map[string]struct{}) error {
	deadline := time.Now().Add(subscriptionTimeout)
	for time.Now().Before(deadline) {
		for _, p := range s.room.GetParticipants() {
			for _, track := range p.Tracks() {
				trackID := track.SID()
				if _, ok := expecting[trackID]; ok {
					if err := s.subscribe(track); err != nil {
						return err
					}

					delete(expecting, track.SID())
					if len(expecting) == 0 {
						return nil
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	for trackID := range expecting {
		return errors.ErrTrackNotFound(trackID)
	}

	return nil
}

func (s *SDKSource) subscribe(track lksdk.TrackPublication) error {
	if pub, ok := track.(*lksdk.RemoteTrackPublication); ok {
		if pub.IsSubscribed() {
			return nil
		}

		logger.Infow("subscribing to track", "trackID", track.SID())

		pub.OnRTCP(s.sync.OnRTCP)
		return pub.SetSubscribed(true)
	}

	return errors.ErrInvalidTrack
}

// ----- Callbacks -----

func (s *SDKSource) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if s.initialized.IsBroken() {
		return
	}

	var onSubscribeErr error
	defer func() {
		if s.initialized.IsBroken() {
			if onSubscribeErr != nil {
				s.OnFailure(onSubscribeErr)
			}
		} else {
			s.errors <- onSubscribeErr
		}
	}()

	s.active.Inc()
	t := s.sync.AddTrack(track, rp.Identity())

	if !s.initialized.IsBroken() {
		s.mu.Lock()
		switch s.RequestType {
		case types.RequestTypeTrackComposite:
			if s.Identity == "" || track.Kind() == webrtc.RTPCodecTypeVideo {
				s.Identity = rp.Identity()
				s.filenameReplacements["{publisher_identity}"] = s.Identity
			}
		case types.RequestTypeTrack:
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				s.TrackKind = "audio"
			} else {
				s.TrackKind = "video"
				// check for video over websocket
				if s.Outputs[types.EgressTypeWebsocket] != nil {
					onSubscribeErr = errors.ErrVideoWebsocket
					return
				}
			}
			s.TrackSource = strings.ToLower(pub.Source().String())

			s.filenameReplacements["{track_id}"] = s.TrackID
			s.filenameReplacements["{track_type}"] = s.TrackKind
			s.filenameReplacements["{track_source}"] = s.TrackSource
			s.filenameReplacements["{publisher_identity}"] = s.Identity
		}
		s.mu.Unlock()
	}

	var codec types.MimeType
	var writeBlanks bool

	switch {
	case strings.EqualFold(track.Codec().MimeType, string(types.MimeTypeOpus)):
		codec = types.MimeTypeOpus

		s.AudioEnabled = true
		s.AudioInCodec = codec
		if s.AudioOutCodec == "" {
			// This should only happen for track egress
			s.AudioOutCodec = codec
		}
		s.AudioTranscoding = true

		if s.RequestType == types.RequestTypeTrack {
			if o := s.GetFileConfig(); o != nil {
				o.OutputType = types.OutputTypeOGG
			}
		}

	case strings.EqualFold(track.Codec().MimeType, string(types.MimeTypeVP8)):
		codec = types.MimeTypeVP8

		s.VideoEnabled = true
		s.VideoInCodec = codec
		if s.VideoOutCodec == "" {
			// This should only happen for track egress
			s.VideoOutCodec = codec
		}
		if s.VideoOutCodec != codec {
			s.VideoTranscoding = true
			writeBlanks = true
		}

		if s.RequestType == types.RequestTypeTrack {
			if o := s.GetFileConfig(); o != nil {
				o.OutputType = types.OutputTypeWebM
			}
		}

	case strings.EqualFold(track.Codec().MimeType, string(types.MimeTypeH264)):
		codec = types.MimeTypeH264

		s.VideoEnabled = true
		s.VideoInCodec = codec
		if s.VideoOutCodec == "" {
			// This should only happen for track egress
			s.VideoOutCodec = types.MimeTypeH264
		}

		if s.RequestType == types.RequestTypeTrack {
			if o := s.GetFileConfig(); o != nil {
				o.OutputType = types.OutputTypeMP4
			}
		}

	default:
		onSubscribeErr = errors.ErrNotSupported(track.Codec().MimeType)
		return
	}

	var logFilename string
	if s.Debug.EnableProfiling {
		if s.Debug.ToUploadConfig() == nil {
			logFilename = path.Join(s.Debug.PathPrefix, fmt.Sprintf("%s.csv", track.ID()))
		} else {
			logFilename = path.Join(s.TmpDir, fmt.Sprintf("%s.csv", track.ID()))
		}
	}

	<-s.GstReady
	src, err := gst.NewElementWithName("appsrc", track.ID())
	if err != nil {
		onSubscribeErr = errors.ErrGstPipelineError(err)
		return
	}

	appSrc := app.SrcFromElement(src)
	writer, err := sdk.NewAppWriter(track, rp, codec, appSrc, s.sync, t, writeBlanks, logFilename)
	if err != nil {
		logger.Errorw("could not create app writer", err)
		onSubscribeErr = err
		return
	}

	ts := &config.TrackSource{
		TrackID: pub.SID(),
		Kind:    pub.Kind(),
		AppSrc:  appSrc,
		Codec:   track.Codec(),
	}

	switch track.Kind() {
	case webrtc.RTPCodecTypeAudio:
		s.audioWriter = writer
		s.AudioTrack = ts
	case webrtc.RTPCodecTypeVideo:
		s.videoWriter = writer
		s.VideoTrack = ts
	}
}

func (s *SDKSource) onTrackMuted(pub lksdk.TrackPublication, _ lksdk.Participant) {
	if w := s.getWriterForTrack(pub.SID()); w != nil {
		w.SetTrackMuted(true)
		if s.OnTrackMuted != nil {
			s.OnTrackMuted(pub.SID())
		}
	}
}

func (s *SDKSource) onTrackUnmuted(pub lksdk.TrackPublication, _ lksdk.Participant) {
	if w := s.getWriterForTrack(pub.SID()); w != nil {
		w.SetTrackMuted(false)
		if s.OnTrackUnmuted != nil {
			s.OnTrackUnmuted(pub.SID())
		}
	}
}

func (s *SDKSource) onTrackUnsubscribed(_ *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
	if w := s.getWriterForTrack(pub.SID()); w != nil {
		w.Drain(true)
		if s.active.Dec() == 0 {
			s.onDisconnected()
		}
	}
}

func (s *SDKSource) getWriterForTrack(trackID string) *sdk.AppWriter {
	if s.audioWriter != nil && s.audioWriter.TrackID() == trackID {
		return s.audioWriter
	}
	if s.videoWriter != nil && s.videoWriter.TrackID() == trackID {
		return s.videoWriter
	}
	return nil
}

func (s *SDKSource) onTrackFinished(trackID string) {
	var w *sdk.AppWriter

	if s.audioWriter != nil && s.audioWriter.TrackID() == trackID {
		logger.Infow("removing audio writer")
		w = s.audioWriter
		s.audioWriter = nil
	} else if s.videoWriter != nil && s.videoWriter.TrackID() == trackID {
		logger.Infow("removing video writer")
		w = s.videoWriter
		s.videoWriter = nil
	} else {
		return
	}

	w.Drain(true)
	if s.active.Dec() == 0 {
		s.onDisconnected()
	}
}

func (s *SDKSource) onDisconnected() {
	select {
	case <-s.endRecording:
		return
	default:
		close(s.endRecording)
	}
}
