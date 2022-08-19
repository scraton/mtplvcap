package mtp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/gorilla/websocket"
	"github.com/paulbellamy/ratecounter"
	"golang.org/x/sync/errgroup"
)

// LVServer captures LV images and serves the images asynchronously.

type LVServer struct {
	Frame        []byte
	newFrameChan chan bool
	frameLock    sync.Mutex

	fpsRate  *ratecounter.RateCounter
	info     InfoPayload
	infoLock sync.Mutex

	upgrader       websocket.Upgrader
	streamClients  map[*websocket.Conn]bool
	streamLock     sync.Mutex
	controlClients map[*websocket.Conn]bool
	controlLock    sync.Mutex
	motionClients  map[*MJPEGResponseWriter]bool
	motionLock     sync.Mutex

	model         Model
	dev           Device
	mtpLock       sync.Mutex
	dummy         bool
	maxResolution bool

	afInterval *atomic.Int64
	afTicker   *MutableTicker
	afNowChan  chan bool

	lrFPS *atomic.Int64

	eg  *errgroup.Group
	ctx context.Context
}

func NewLVServer(ctx context.Context, dev Device, maxResolution bool) *LVServer {
	eg, egCtx := errgroup.WithContext(ctx)

	return &LVServer{
		Frame:        nil,
		newFrameChan: make(chan bool, 1),

		fpsRate: ratecounter.NewRateCounter(time.Second),

		streamClients:  map[*websocket.Conn]bool{},
		controlClients: map[*websocket.Conn]bool{},
		motionClients:  map[*MJPEGResponseWriter]bool{},

		dev:   dev,
		dummy: dev == nil,

		maxResolution: maxResolution,

		afInterval: atomic.NewInt64(5),
		afTicker:   NewMutableTicker(5 * time.Second),
		afNowChan:  make(chan bool),

		lrFPS: atomic.NewInt64(0),

		eg:  eg,
		ctx: egCtx,
	}
}

// HTTP handler / WebSocket

func (s *LVServer) HandleStream(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.LV.Errorf("HandleStream: failed to upgrade: %s", err)
	}
	defer ws.Close()

	s.registerStreamClient(ws)
	for {
		var mes struct{}
		err := ws.ReadJSON(&mes)
		if err != nil {
			log.LV.Errorf("HandleStream: failed to read a message: %s", err)
			s.unregisterStreamClient(ws)
			return
		}
	}
}

func (s *LVServer) registerStreamClient(c *websocket.Conn) {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	s.streamClients[c] = true
}

func (s *LVServer) unregisterStreamClient(c *websocket.Conn) {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	delete(s.streamClients, c)
}

type ControlPayload struct {
	AFInterval *int64  `json:"af_interval,omitempty"`
	AFFocusNow *bool   `json:"af_focus_now,omitempty"`
	LRFPS      *int64  `json:"lr_fps,omitempty"`
	ISO        *int    `json:"iso,omitempty"`
	FN         *string `json:"fn,omitempty"`
}

type InfoPayload struct {
	ISO    int      `json:"iso"`
	ISOs   []int    `json:"isos"`
	FN     string   `json:"fn"`
	FNs    []string `json:"fns"`
	AF     int64    `json:"af"`
	LR     int64    `json:"lr"`
	Width  int      `json:"width"`
	Height int      `json:"height"`
	FPS    int      `json:"fps"`
	Frame  []byte   `json:"frame"`
}

func (s *LVServer) HandleControl(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.LV.Errorf("HandleControl: failed to upgrade: %s", err)
	}
	defer ws.Close()

	setInfo := func(af *int64, lr *int64) {
		s.infoLock.Lock()
		defer s.infoLock.Unlock()
		if af != nil {
			s.info.AF = *af
		}
		if lr != nil {
			s.info.LR = *lr
		}
	}

	s.registerControlClient(ws)
	for {
		var p ControlPayload
		err := ws.ReadJSON(&p)
		if err != nil {
			log.LV.Errorf("HandleControl: failed to read a message: %s", err)
			s.unregisterControlClient(ws)
			return
		}

		if p.AFInterval != nil {
			setInfo(p.AFInterval, nil)

			if *p.AFInterval > 0 {
				log.LV.Debug("HandleControl: enable AF")
				s.afTicker.Start()
			} else {
				log.LV.Debug("HandleControl: disable AF")
				s.afTicker.Stop()
				continue
			}

			s.afInterval.Store(*p.AFInterval)
			s.afTicker.SetInterval(time.Duration(*p.AFInterval) * time.Second)
			if err != nil {
				log.LV.Debugf("HandleControl: failed to set interval: %d", *p.AFInterval)
			}
			log.LV.Debugf("HandleControl: set AF interval: %d", *p.AFInterval)
		}

		if p.AFFocusNow != nil && *p.AFFocusNow {
			log.LV.Debug("HandleControl: focus now")
			select {
			case s.afNowChan <- true:
			default:
			}
		}

		if p.LRFPS != nil {
			setInfo(nil, p.LRFPS)
			if *p.LRFPS > 0 {
				log.LV.Debugf("HandleControl: set rate limit: %d", *p.LRFPS)
			} else {
				log.LV.Debug("HandleControl: disable rate limit")
			}
			s.lrFPS.Store(*p.LRFPS)
		}

		if p.ISO != nil {
			log.LV.Debugf("HandleControl: set ISO: %d", *p.ISO)
			err = s.setISO(*p.ISO)
			if err != nil {
				log.LV.Errorf("HandleControl: failed to set ISO: %s", err)
			}
		}

		if p.FN != nil {
			log.LV.Debugf("HandleControl: set f-number: %s", *p.FN)
			err = s.setFN(*p.FN)
			if err != nil {
				log.LV.Errorf("HandleControl: failed to set f-number: %s", err)
			}
		}
	}
}

func (s *LVServer) registerControlClient(c *websocket.Conn) {
	s.controlLock.Lock()
	defer s.controlLock.Unlock()
	s.controlClients[c] = true
}

func (s *LVServer) unregisterControlClient(c *websocket.Conn) {
	s.controlLock.Lock()
	defer s.controlLock.Unlock()
	delete(s.controlClients, c)
}

func (s *LVServer) HandleMotionJPEG(w http.ResponseWriter, r *http.Request) {
	log.LV.Info("handling GET /mjpeg")

	writer := NewMJPEGResponseWriter(w)
	s.registerMotionClient(writer)

	<-r.Context().Done()

	s.unregisterMotionClient(writer)
}

func (s *LVServer) registerMotionClient(w *MJPEGResponseWriter) {
	s.controlLock.Lock()
	defer s.controlLock.Unlock()
	s.motionClients[w] = true
}

func (s *LVServer) unregisterMotionClient(w *MJPEGResponseWriter) {
	s.motionLock.Lock()
	defer s.motionLock.Unlock()
	delete(s.motionClients, w)
}

func (s *LVServer) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	var jpeg []byte
	jpeg = s.copyFrame()

	writer := NewSnapshotResponseWriter(w)
	writer.Write(jpeg)
}

// Workers

func (s *LVServer) Run() error {
	defer func() {
		_ = s.endLiveView()
	}()

	id, err := s.dev.ID()
	if err != nil {
		log.LV.Fatalf("failed to get device identity: %s", err)
	}

	log.LV.Debugf(
		"manufacturer = %s, product = %s, serialnumber = %s",
		id.Manufacturer,
		id.Product,
		id.SerialNumber,
	)

	model, ok := models.Match(id.Product)
	if ok {
		log.LV.Debugf("model matched: %s", model.Name)
	} else {
		log.LV.Debugf("model didn't match, falling back to the generic model %s", model.Name)
		model = models.Generic()
	}
	s.model = model

	isos, _, err := s.getISOs()
	if err != nil {
		log.LV.Warningf("failed to obtain ISO list: %s", err)
		isos = []int{0}
	}
	s.info.ISOs = isos

	fns, _, err := s.getFNs()
	if err != nil {
		log.LV.Warningf("failed to obtain F-values: %s", err)
		fns = []string{"0"}
	}
	s.info.FNs = fns

	s.eg.Go(s.workerLV)
	s.eg.Go(s.workerAF)
	time.Sleep(500 * time.Millisecond)
	s.eg.Go(s.frameCaptorSakura)
	s.eg.Go(s.workerBroadcastFrame)
	s.eg.Go(s.workerBroadcastInfo)
	return s.eg.Wait()
}

func (s *LVServer) workerLV() error {
	tick := time.NewTicker(time.Second)

	for {
		select {
		case <-tick.C:
			// let's go!
		case <-s.ctx.Done():
			return nil
		}

		status, err := s.getLiveViewStatus()
		if err != nil {
			log.LV.Warningf("workerLV: %s", err)
			continue
		} else if status {
			continue
		}

		err = s.startLiveView()
		if err != nil {
			log.LV.Warningf("workerLV: %s", err)
		}
	}
}

func (s *LVServer) workerAF() error {
	for {
		select {
		case <-s.afTicker.C:
			// Let's go!
		case <-s.afNowChan:
			// Do it now
		case <-s.ctx.Done():
			return nil
		}

		err := s.autoFocus()
		if err != nil {
			log.LV.Warningf("workerAF: %s", err)
		}
	}
}

func (s *LVServer) frameCaptorSakura() error {
	set := func(lv LiveView, iso int, fn string) {
		s.frameLock.Lock()
		s.infoLock.Lock()
		defer s.frameLock.Unlock()
		defer s.infoLock.Unlock()
		s.Frame = lv.JPEG
		s.info.Width = int(lv.LVWidth)
		s.info.Height = int(lv.LVHeight)
		s.info.ISO = iso
		s.info.FN = fn
		select {
		case s.newFrameChan <- true:
		default:
		}
	}

	last := time.Now()

	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
			// Let's go!
		}

		if s.dummy {
			time.Sleep(time.Second)
			continue
		}

		if s.lrFPS.Load() > 0 {
			time.Sleep(last.Add(time.Second / time.Duration(s.lrFPS.Load())).Sub(time.Now()))
		}
		last = time.Now()

		lv, err := s.getLiveViewImg()
		if err != nil {
			if err.Error() == "failed to obtain an image: live view is not activated" {
				time.Sleep(time.Second)
				continue
			} else {
				log.LV.Warningf("frameCaptor: %s", err)
				time.Sleep(time.Second)
				continue
			}
		}
		_, currentISO, err := s.getISOs()
		if err != nil {
			log.LV.Warningf("frameCaptor: failed to get current ISO: %s", err)
			currentISO = 0
		}

		_, currentFN, err := s.getFNs()
		if err != nil {
			log.LV.Warningf("frameCaptor: failed to get current f-number: %s", err)
			currentFN = "0"
		}

		set(lv, currentISO, currentFN)
		s.fpsRate.Incr(1)
	}
}

func (s *LVServer) copyFrame() []byte {
	s.frameLock.Lock()
	defer s.frameLock.Unlock()
	return s.Frame[:]
}

func (s *LVServer) workerBroadcastFrame() error {
	broadcast := func(jpeg []byte) {
		s.streamLock.Lock()
		defer s.streamLock.Unlock()

		s.motionLock.Lock()
		defer s.motionLock.Unlock()

		b64 := base64.StdEncoding.EncodeToString(jpeg)

		for c := range s.streamClients {
			err := c.WriteMessage(websocket.TextMessage, []byte(b64))
			if err != nil {
				log.LV.Errorf("workerBroadcastFrame: failed to send a frame: %s", err)
			}
		}

		for w := range s.motionClients {
			err := w.Write(jpeg)
			if err != nil {
				log.LV.Errorf("workerBroadcastFrame: failed to send a frame: %s", err)
			}
		}
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		case <-s.newFrameChan:
		}

		var jpeg []byte
		jpeg = s.copyFrame()
		if len(jpeg) == 0 {
			continue
		}
		broadcast(jpeg)
	}
}

func (s *LVServer) workerBroadcastInfo() error {
	tick := time.NewTicker(time.Second)

	broadcast := func() {
		s.controlLock.Lock()
		s.infoLock.Lock()
		defer s.controlLock.Unlock()
		defer s.infoLock.Unlock()

		s.info.Frame = s.copyFrame()
		s.info.FPS = int(s.fpsRate.Rate())

		for c := range s.controlClients {
			j, err := json.Marshal(s.info)
			if err != nil {
				log.LV.Errorf("workerBroadcastInfo: failed to marshal payload: %s", err)
				continue
			}
			err = c.WriteMessage(websocket.TextMessage, j)
			if err != nil {
				log.LV.Errorf("workerBroadcastInfo: failed to send a frame: %s", err)
			}
		}
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		case <-tick.C:
			// Let's go!
		}

		broadcast()
	}
}

// Thread-safe MTP communication

func (s *LVServer) startLiveView() error {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	err := s.dev.RunTransactionWithNoParams(OC_NIKON_DeviceReady)
	if err != nil {
		return fmt.Errorf("failed to start live view: the camera is not ready")
	}

	if s.model.QuirkSwitchMedia {
		err = s.switchRecordMedia()
		if err != nil {
			return fmt.Errorf("failed to switch recording media: %s", err)
		}
	}

	if s.maxResolution {
		err = s.changeResolution()
		if err != nil {
			log.LV.Warningf("failed to change the image resolution (%s); if it affects capturing frames, consider disabling `-max-resolution`", err)
		}
	}

	err = s.dev.RunTransactionWithNoParams(OC_NIKON_StartLiveView)
	if err != nil {
		if casted, ok := err.(RCError); ok && uint16(casted) == RC_NIKON_InvalidStatus {
			log.LV.Error("failed to start live view (InvalidStatus). Investigating the reason...")
			reason, err := s.readLiveViewProhibitCondition()
			if err != nil {
				return fmt.Errorf("failed to start live view and failed to investigate the reason: %s", err)
			}
			return fmt.Errorf("failed to start live view, reason: %s", reason)
		}
		return fmt.Errorf("failed to start live view: %s", err)
	}
	return nil
}

func (s *LVServer) switchRecordMedia() error {
	desc := DevicePropDesc{}
	err := s.dev.GetDevicePropDesc(DPC_NIKON_RecordingMedia, &desc)
	if err != nil {
		return fmt.Errorf("failed to get recording media: %s", err)
	}

	if currentMedia, ok := desc.CurrentValue.(int8); ok {
		if currentMedia == int8(RecordingMediaCard) {
			log.LV.Debug("current recording media: card")
			log.LV.Debug("the recording media is the card. Switching it to the SDRAM.")
			payload := struct {
				Media RecordingMedia
			}{
				Media: RecordingMediaSDRAM,
			}
			err = s.dev.SetDevicePropValue(DPC_NIKON_RecordingMedia, &payload)
			if err != nil {
				return fmt.Errorf("failed to SetDevicePropValue: %s", err)
			}
		} else {
			log.LV.Debug("current recording media: SDRAM")
		}
	} else {
		log.LV.Warning("unexpected format of the RecordingMedia property")
	}
	return nil
}

func (s *LVServer) changeResolution() error {
	log.LV.Infof("getting available resolutions")
	desc := DevicePropDesc{}
	err := s.dev.GetDevicePropDesc(DPC_NIKON_Resolution, &desc)
	if err != nil {
		return fmt.Errorf("failed to get recording media: %s", err)
	}

	values, ok := desc.Form.(*PropDescEnumForm)
	if !ok {
		return fmt.Errorf("failed to assert returned value (DPC_NIKON_Resolution)")
	}

	var choices []uint64
	for _, iface := range values.Values {
		v, ok := iface.(uint64)
		if !ok {
			return fmt.Errorf("failed to assert a value in the array as uint64")
		}
		choices = append(choices, v)
	}

	log.LV.Infof("available resolutions (higher is larger): %v", choices)
	log.LV.Infof("automatically use the largest choice: %d", choices[len(choices)-1])

	switch s.model.ResolutionType {
	case ResolutionType64:
		payload := struct {
			Resolution Resolution64
		}{
			Resolution: Resolution64(choices[len(choices)-1]),
		}

		err = s.dev.SetDevicePropValue(DPC_NIKON_Resolution, &payload)
		if err != nil {
			return fmt.Errorf("failed to SetDevicePropValue: %s", err)
		}
	case ResolutionType8:
		payload := struct {
			Resolution Resolution8
		}{
			Resolution: Resolution8(choices[len(choices)-1]),
		}

		err = s.dev.SetDevicePropValue(DPC_NIKON_Resolution, &payload)
		if err != nil {
			return fmt.Errorf("failed to SetDevicePropValue: %s", err)
		}
	}

	return nil
}

func (s *LVServer) readLiveViewProhibitCondition() (string, error) {
	// mtpLock must be locked by caller
	var reasonRaw Uint32Value
	err := s.dev.GetDevicePropValue(DPC_NIKON_LiveViewProhibitCondition, &reasonRaw)
	if err != nil {
		return "", fmt.Errorf("failed to read LiveViewProhibitCondition: %s", err)
	}

	switch s.bitScan(reasonRaw.Value) {
	case -1:
		return "(empty)", nil
	case 0:
		return "recording destination is the card", nil
	case 2:
		return "sequence error", nil
	case 4:
		return "button is fully pressed", nil
	case 5:
		return "aperture value is set by the lens", nil
	case 6:
		return "bulb error", nil
	case 7:
		return "during cleaning", nil
	case 8:
		return "insufficient battery", nil
	case 9:
		return "TTL error", nil
	case 11:
		return "non-CPU lens is mounted and the mode is not M", nil
	case 12:
		return "there are images which are recorded in SDRAM", nil
	case 13:
		return "the release mode is mirror-up", nil
	case 14:
		return "no card inserted", nil
	case 15:
		return "shot command is being processed", nil
	case 16:
		return "shooting in progress", nil
	case 17:
		return "overheated", nil
	case 18:
		return "card is protected", nil
	case 19:
		return "card error", nil
	case 20:
		return "card is not formatted", nil
	case 21:
		return "bulb error", nil
	case 22:
		return "the release mode is mirror-up and it is being processed", nil
	case 24:
		return "the lens is not extended", nil
	default:
		return "unknown reason", nil
	}

}

func (*LVServer) bitScan(val uint32) int {
	for i := 0; i < 64; i++ {
		if val&(1<<i) > 0 {
			return i
		}
	}
	return -1
}

func (s *LVServer) endLiveView() error {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return nil
	}

	err := s.dev.RunTransactionWithNoParams(OC_NIKON_EndLiveView)
	if err != nil {
		return fmt.Errorf("failed to end live view: %s", err)
	}
	return nil
}

func (s *LVServer) getLiveViewStatus() (bool, error) {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return true, nil
	}

	err, status := s.getLiveViewStatusInner()
	if err != nil {
		return false, fmt.Errorf("failed to get live view status: %s", err)
	}
	return status, nil
}

func (s *LVServer) getLiveViewStatusInner() (error, bool) {
	val := StringValue{}
	err := s.dev.GetDevicePropValue(DPC_NIKON_LiveViewStatus, &val)

	if err != nil && err != io.EOF {
		return err, false
	}

	return nil, err == io.EOF
}

func (s *LVServer) autoFocus() error {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return nil
	}

	err := s.dev.RunTransactionWithNoParams(OC_NIKON_AfDrive)
	if err != nil {
		return fmt.Errorf("failed to do auto focus: %s", err)
	}
	return nil
}

func (s *LVServer) getLiveViewImg() (LiveView, error) {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return LiveView{}, nil
	}

	lv, err := s.getLiveViewImgInner()
	if err != nil {
		return LiveView{}, err
	}
	return lv, nil
}

type liveViewRaw struct {
	LVWidth             int16
	LVHeight            int16
	Width               int16
	Height              int16
	Dummy1              [8]byte
	FocusFrameWidth     int16
	FocusFrameHeight    int16
	FocusX              int16
	FocusY              int16
	Dummy2              [5]byte
	Rotation            int8
	Dummy3              [10]byte
	AutoFocus           int8
	Dummy4              [15]byte
	MovieTimeRemainInt  int16
	MovieTimeRemainFrac int16
	Recording           int8
}

type LiveView struct {
	LVWidth          int16
	LVHeight         int16
	Width            int16
	Height           int16
	FocusFrameWidth  int16
	FocusFrameHeight int16
	FocusX           int16
	FocusY           int16
	Rotation         Rotation
	AutoFocus        AF
	Recording        bool

	JPEG []byte
}

func (s *LVServer) getLiveViewImgInner() (LiveView, error) {
	var req, rep Container
	buf := bytes.NewBuffer([]byte{})

	hs := s.model.HeaderSize

	req.Code = OC_NIKON_GetLiveViewImg
	req.Param = []uint32{}
	err := s.dev.RunTransaction(&req, &rep, buf, nil, 0)
	if err != nil {
		if casted, ok := err.(RCError); ok && uint16(casted) == RC_NIKON_NotLiveView {
			return LiveView{}, fmt.Errorf("failed to obtain an image: live view is not activated")
		}
		return LiveView{}, fmt.Errorf("failed to obtain an image: %s", err)
	} else if buf.Len() <= hs {
		return LiveView{}, fmt.Errorf("failed to obtain an image: the data has insufficient length")
	}

	raw := buf.Bytes()

	lvr := liveViewRaw{}
	err = binary.Read(bytes.NewReader(raw[8:hs]), binary.BigEndian, &lvr)
	if err != nil {
		return LiveView{}, fmt.Errorf("failed to decode header")
	}

	rot := Rotation0
	if lvr.Rotation == 1 {
		rot = RotationMinus90
	} else if lvr.Rotation == 2 {
		rot = Rotation90
	} else if lvr.Rotation == 3 {
		rot = Rotation180
	}

	af := AFNotActive
	if lvr.AutoFocus == 1 {
		af = AFFail
	} else if lvr.AutoFocus == 2 {
		af = AFSuccess
	}

	return LiveView{
		LVWidth:          lvr.LVWidth,
		LVHeight:         lvr.LVHeight,
		Width:            lvr.Width,
		Height:           lvr.Height,
		FocusFrameWidth:  lvr.FocusFrameWidth,
		FocusFrameHeight: lvr.FocusFrameHeight,
		FocusX:           lvr.FocusX,
		FocusY:           lvr.FocusY,
		Rotation:         rot,
		AutoFocus:        af,
		Recording:        lvr.Recording == 1,
		JPEG:             raw[hs:],
	}, nil
}

func (s *LVServer) getISOs() ([]int, int, error) {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return []int{100, 1000, 10000}, 100, nil
	}

	isoi := make([]int, 0)

	val := DevicePropDesc{}
	err := s.dev.GetDevicePropDesc(DPC_ExposureIndex, &val)

	if err != nil && err != io.EOF {
		return isoi, 0, err
	}

	asserted, ok := val.Form.(*PropDescEnumForm)
	if !ok {
		return isoi, 0, fmt.Errorf("unexpedted type: could not assert that returned prop is enum form")
	}

	for _, raw := range asserted.Values {
		iso, ok := raw.(uint64)
		if !ok {
			return isoi, 0, fmt.Errorf("unexpedted type: could not assert that form value is uint64")
		}
		isoi = append(isoi, int(iso))
	}

	currentISO, ok := val.CurrentValue.(uint16)
	if !ok {
		return isoi, 0, fmt.Errorf("unexpedted type: could not assert that current value is uint16")
	}

	return isoi, int(currentISO), nil
}

func (s *LVServer) setISO(iso int) error {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return nil
	}

	err := s.dev.SetDevicePropValue(DPC_ExposureIndex, &struct {
		ISO uint16
	}{
		ISO: uint16(iso),
	})
	if err != nil {
		return fmt.Errorf("failed to set ISO: %s", err)
	}
	return nil
}

func (s *LVServer) getFNs() ([]string, string, error) {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return []string{"3.5", "10", "22"}, "3.5", nil
	}

	fns := make([]string, 0)

	val := DevicePropDesc{}
	err := s.dev.GetDevicePropDesc(DPC_FNumber, &val)

	if err != nil && err != io.EOF {
		return fns, "", err
	}

	asserted, ok := val.Form.(*PropDescEnumForm)
	if !ok {
		return fns, "", fmt.Errorf("unexpedted type: could not assert that returned prop is enum form")
	}

	for _, raw := range asserted.Values {
		fn, ok := raw.(uint64)
		if !ok {
			return fns, "", fmt.Errorf("unexpedted type: could not assert that form value is uint64")
		}
		fns = append(fns, strconv.FormatFloat(float64(fn)/100, 'f', -1, 64))
	}

	current, ok := val.CurrentValue.(uint16)
	if !ok {
		return fns, "", fmt.Errorf("unexpedted type: could not assert that current value is uint16")
	}

	return fns, strconv.FormatFloat(float64(current)/100, 'f', -1, 64), nil
}

func (s *LVServer) setFN(fn string) error {
	s.mtpLock.Lock()
	defer s.mtpLock.Unlock()

	if s.dummy {
		return nil
	}

	fnf, err := strconv.ParseFloat(fn, 64)
	if err != nil {
		return fmt.Errorf("failed to parse f-number: %s", err)
	}

	err = s.dev.RunTransactionWithNoParams(OC_NIKON_EndLiveView)
	if err != nil {
		return fmt.Errorf("failed to set f-number: failed to stop live view: %s", err)
	}

	err = s.dev.SetDevicePropValue(DPC_FNumber, &struct {
		FN uint16
	}{
		FN: uint16(fnf * 100),
	})
	if err != nil {
		return fmt.Errorf("failed to set f-number: %s", err)
	}

	err = s.dev.RunTransactionWithNoParams(OC_NIKON_StartLiveView)
	if err != nil {
		if casted, ok := err.(RCError); ok && uint16(casted) == RC_NIKON_InvalidStatus {
			return fmt.Errorf("failed to set f-number: failed to start live view: InvalidStatus (battery level is low?)")
		}
		return fmt.Errorf("failed to set f-number: start live view: %s", err)
	}
	return nil
}
