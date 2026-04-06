package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type WeatherStationServer struct {
	config *Config
	clock  Clock
	al     Alerter
	db     *sql.DB
	wu     *WUClient
}

func (ws *WeatherStationServer) backgroundLoop() {
	// Start weather polling in a separate goroutine
	if ws.wu != nil {
		go ws.weatherPollLoop()
	}

	// Image capture loop
	var lastAlertDay string
	for i := int64(0); ; i++ {
		slog.Debug("running image capture loop", "iteration", i)
		if err := ws.captureAndOverlay(); err == nil {
			time.Sleep(time.Second * time.Duration(ws.config.RefreshSecs))
			continue
		} else {
			now := ws.clock.NowPacific()
			today := now.Format(time.DateOnly)
			if today != lastAlertDay {
				ws.al.Fire("Error refreshing WS image", fmt.Sprintf("err: %+v", err))
				lastAlertDay = today
			}
			time.Sleep(time.Minute)
		}
	}
}

// weatherPollLoop fetches weather data from WU every 5 minutes and stores it in SQLite.
func (ws *WeatherStationServer) weatherPollLoop() {
	// Fetch immediately on startup
	ws.fetchAndStoreWeather()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ws.fetchAndStoreWeather()
	}
}

func (ws *WeatherStationServer) fetchAndStoreWeather() {
	obs, err := ws.wu.FetchCurrent()
	if err != nil {
		slog.Error("failed to fetch weather", "err", err)
		return
	}
	if err := InsertObservation(ws.db, obs); err != nil {
		slog.Error("failed to store observation", "err", err)
		return
	}
	slog.Info("weather updated",
		"temp", obs.Temp,
		"humidity", obs.Humidity,
		"wind", obs.WindSpeed,
		"precip", obs.PrecipRate)
}

// captureAndOverlay captures an RTSP frame, overlays weather data, and saves it.
func (ws *WeatherStationServer) captureAndOverlay() (err error) {
	now := ws.clock.NowPacific()

	// Ensure image directory exists for today
	relPath := imagePath(now)
	absPath := filepath.Join(ws.config.ImageDir, relPath)
	if err = os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return Wrap(err, "creating image directory")
	}

	// Capture RTSP frame
	ffmpegCmd := fmt.Sprintf(
		"ffmpeg -y -rtsp_transport tcp -i %s -qscale:v 3 -frames:v 1 %s",
		ws.config.RTSPStream, absPath)
	cmd := exec.Command("bash", "-c", ffmpegCmd)
	cmdOutput, err := cmd.CombinedOutput()
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to capture RTSP frame")
		return
	}

	// Fallback: if overlay fails, still copy raw frame as current.jpg
	currentPath := filepath.Join(ws.config.ImageDir, "current.jpg")
	defer func() {
		if err == nil {
			return
		}
		// Just copy the raw capture as the current image with a timestamp
		timeStr := now.Format("2006-01-02   3:04PM")
		addTimeCmd := fmt.Sprintf(
			`convert %s -font Helvetica -antialias -gravity NorthWest `+
				`-fill '#aaaaaa' -pointsize 50 `+
				`-annotate +20+5 '%v' %s`,
			absPath, timeStr, currentPath)
		cmd = exec.Command("bash", "-c", addTimeCmd)
		cmdOutput, innerErr := cmd.CombinedOutput()
		if innerErr != nil {
			slog.Error("failed to write timestamp fallback", "err", innerErr, "output", string(cmdOutput))
		}
	}()

	// Read latest weather from DB
	obs, dbErr := LatestObservation(ws.db)
	if dbErr != nil || obs == nil {
		err = Errorf("no weather data available yet (db err: %v)", dbErr)
		return
	}

	// Overlay weather data on image
	timeStr := now.Format("2006-01-02   3:04PM")
	temp := math.Round(obs.Temp)
	feelsLike := math.Round(obs.FeelsLike)
	wind := math.Round(obs.WindSpeed)
	windGust := math.Round(obs.WindGust)
	humidity := math.Round(obs.Humidity)
	precip := obs.PrecipRate
	pressure := obs.Pressure

	lineHeight := 60
	imageMagickCmd := fmt.Sprintf(
		`convert %s -font Helvetica -antialias -gravity NorthWest `+
			`-fill '#aaaaaa' -pointsize 50 `+
			`-annotate +20+5 '%v' `+
			`-annotate +20+%d 'Temp: %.0fF (feels-like %.0fF)' `+
			`-annotate +20+%d 'Wind: %.0f mph (Gusts: %.0f mph)' `+
			`-annotate +20+%d 'Humidity: %.0f%%  Rain: %.2f in/hr' `+
			`-annotate +20+%d 'Pressure: %.2f inHg' %s`,
		absPath, timeStr,
		5+lineHeight, temp, feelsLike,
		5+2*lineHeight, wind, windGust,
		5+3*lineHeight, humidity, precip,
		5+4*lineHeight, pressure,
		currentPath)
	cmd = exec.Command("bash", "-c", imageMagickCmd)
	cmdOutput, err = cmd.CombinedOutput()
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to overlay weather data")
		return
	}

	// Register image in DB
	if dbErr := InsertImage(ws.db, &ImageRecord{
		Timestamp: now.Unix(),
		Path:      relPath,
	}); dbErr != nil {
		slog.Error("failed to register image in DB", "err", dbErr)
	}

	return nil
}
