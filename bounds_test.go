package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCapImages(t *testing.T) {
	mk := func(ts int64, pinned bool) ImageRecord { return ImageRecord{Timestamp: ts, Pinned: pinned} }

	// Small set: returned unchanged.
	small := []ImageRecord{mk(1, false), mk(2, false)}
	if got := capImages(small, maxViewerFrames); len(got) != 2 {
		t.Errorf("small passthrough len = %d, want 2", len(got))
	}

	// Large set: bounded regardless of size; order preserved; pinned always kept
	// even when it falls off the stride.
	var imgs []ImageRecord
	const pinnedTS = 1234 // 1234 % ceil(2000/500)=4 != 0, so only pinned keeps it
	for i := range 2000 {
		imgs = append(imgs, mk(int64(i), i == pinnedTS))
	}
	got := capImages(imgs, maxViewerFrames)
	if len(got) > maxViewerFrames+1 {
		t.Errorf("capped len = %d, want <= %d", len(got), maxViewerFrames+1)
	}
	if len(got) < maxViewerFrames/2 {
		t.Errorf("capped len = %d, unexpectedly small", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Fatalf("cap did not preserve ascending order at %d", i)
		}
	}
	foundPinned := false
	for _, im := range got {
		if im.Timestamp == pinnedTS {
			foundPinned = true
			if !im.Pinned {
				t.Error("pinned flag lost")
			}
		}
	}
	if !foundPinned {
		t.Error("off-stride pinned frame was dropped by the cap")
	}
}

func TestDownsampleObservations(t *testing.T) {
	// Small set: returned unchanged.
	small := []Observation{{Timestamp: 1}, {Timestamp: 2}}
	if got := downsampleObservations(small, maxChartPoints); len(got) != 2 {
		t.Errorf("small passthrough len = %d, want 2", len(got))
	}

	// Large set: bounded, first point kept, ascending preserved.
	var obs []Observation
	for i := range 5000 {
		obs = append(obs, Observation{Timestamp: int64(i)})
	}
	got := downsampleObservations(obs, maxChartPoints)
	if len(got) == 0 || len(got) > maxChartPoints {
		t.Errorf("len = %d, want 1..%d", len(got), maxChartPoints)
	}
	if got[0].Timestamp != 0 {
		t.Errorf("first ts = %d, want 0", got[0].Timestamp)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Fatalf("downsample did not preserve ascending order at %d", i)
		}
	}
}

func TestAttachWeather(t *testing.T) {
	imgs := []ImageRecord{{Timestamp: 100}, {Timestamp: 250}, {Timestamp: 5000}}
	obs := []Observation{{Timestamp: 90, Temp: 1}, {Timestamp: 300, Temp: 2}}
	got := attachWeather(imgs, obs)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// 100 is nearest 90 (dist 10) not 300 (dist 200).
	if got[0].Weather == nil || got[0].Weather.Temp != 1 {
		t.Errorf("img0 weather = %+v, want temp 1", got[0].Weather)
	}
	// 250 is nearest 300 (dist 50) not 90 (dist 160).
	if got[1].Weather == nil || got[1].Weather.Temp != 2 {
		t.Errorf("img1 weather = %+v, want temp 2", got[1].Weather)
	}
	// 5000 is >weatherMatchWindow from any observation -> nil, not a stale match.
	if got[2].Weather != nil {
		t.Errorf("img2 weather = %+v, want nil (out of window)", got[2].Weather)
	}
	// Embedded ImageRecord fields survive.
	if got[0].Timestamp != 100 {
		t.Errorf("embedded timestamp lost: %d", got[0].Timestamp)
	}
	// No observations: every frame nil, no panic.
	for i, f := range attachWeather(imgs, nil) {
		if f.Weather != nil {
			t.Errorf("nil-obs weather[%d] = %+v, want nil", i, f.Weather)
		}
	}
}

// TestAPIObservationsCap confirms the handler caps points regardless of range.
func TestAPIObservationsCap(t *testing.T) {
	wss, mux := newTestServer(t)
	base := time.Now().Unix() - int64(maxChartPoints)*60
	n := maxChartPoints + 200
	for i := range n {
		InsertObservation(wss.db, &Observation{Timestamp: base + int64(i)*60, Temp: float64(i)})
	}
	req := httptest.NewRequest("GET", "/api/observations?from="+itoa(base-1)+"&to="+itoa(base+int64(n)*60), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Observations []Observation `json:"observations"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Observations) == 0 || len(resp.Observations) > maxChartPoints {
		t.Errorf("returned %d observations, want 1..%d", len(resp.Observations), maxChartPoints)
	}
}

// TestAPIImagesCap confirms /api/images bounds the frame count for a dense day.
func TestAPIImagesCap(t *testing.T) {
	wss, mux := newTestServer(t)
	day := time.Now().In(ptLocation)
	base := time.Date(day.Year(), day.Month(), day.Day(), 1, 0, 0, 0, ptLocation)
	for i := range maxViewerFrames + 200 {
		ts := base.Add(time.Duration(i) * 30 * time.Second)
		InsertImage(wss.db, &ImageRecord{Timestamp: ts.Unix(), Path: imagePath(ts)})
	}
	req := httptest.NewRequest("GET", "/api/images?date="+day.Format("2006-01-02"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Images []json.RawMessage `json:"images"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Images) == 0 || len(resp.Images) > maxViewerFrames {
		t.Errorf("returned %d frames, want 1..%d", len(resp.Images), maxViewerFrames)
	}
}

// TestAPIImagesWeatherAttached confirms each frame carries its nearest weather,
// so the viewer needs no per-frame /api/nearest-observation call.
func TestAPIImagesWeatherAttached(t *testing.T) {
	wss, mux := newTestServer(t)
	day := time.Now().In(ptLocation)
	f0 := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, ptLocation).Unix()
	f1 := f0 + 600 // 10 min later
	InsertImage(wss.db, &ImageRecord{Timestamp: f0, Path: imagePath(time.Unix(f0, 0).In(ptLocation))})
	InsertImage(wss.db, &ImageRecord{Timestamp: f1, Path: imagePath(time.Unix(f1, 0).In(ptLocation))})
	InsertObservation(wss.db, &Observation{Timestamp: f0 + 30, Temp: 41})
	InsertObservation(wss.db, &Observation{Timestamp: f1 - 20, Temp: 62})

	req := httptest.NewRequest("GET", "/api/images?date="+day.Format("2006-01-02"), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Images []struct {
			Timestamp int64        `json:"timestamp"`
			Weather   *Observation `json:"weather"`
		} `json:"images"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(resp.Images))
	}
	if resp.Images[0].Weather == nil || resp.Images[0].Weather.Temp != 41 {
		t.Errorf("frame0 weather = %+v, want temp 41", resp.Images[0].Weather)
	}
	if resp.Images[1].Weather == nil || resp.Images[1].Weather.Temp != 62 {
		t.Errorf("frame1 weather = %+v, want temp 62", resp.Images[1].Weather)
	}
}
