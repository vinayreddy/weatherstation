package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os/exec"
	"time"
)

type WeatherStationServer struct {
	config          *Config
	clock           Clock
	al              Alerter
	lastWeatherData *weatherData
}

type weatherData struct {
	owm       map[string]any
	timestamp int64
}

func (ws *WeatherStationServer) backgroundLoop() {
	var lastAlertDay string

	for i := int64(0); ; i++ {
		slog.Debug("running background loop", "iteration", i)
		if err := ws.generateImage(); err == nil {
			time.Sleep(time.Second * time.Duration(ws.config.RefreshSecs))
			continue
		} else {
			now := ws.clock.NowPacific()
			today := now.Format(time.DateOnly)
			if today != lastAlertDay {
				ws.al.Fire("Error refreshing WS image", fmt.Sprintf("err: %+v", err))
				lastAlertDay = today
			}
			// Wait a bit after a failure.
			time.Sleep(time.Minute)
		}
	}
}

func (ws *WeatherStationServer) generateImage() (err error) {
	ffmpegCmd := fmt.Sprintf(
		"ffmpeg -y -rtsp_transport tcp -i %s -qscale:v 3 -frames:v 1 %s/ffmpeg.jpg",
		ws.config.RTSPStream, ws.config.ImageDir)
	cmd := exec.Command("bash", "-c", ffmpegCmd)
	cmdOutput, err := cmd.CombinedOutput()
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to read from weather station stream")
		return
	}

	// Fallback: if weather data or overlay fails, still write timestamp-only image.
	defer func() {
		if err == nil {
			return
		}
		timeStr := ws.clock.NowPacific().Format("2006-01-02   3:04PM")
		addTimeCmd := fmt.Sprintf(
			`pushd %s && convert ffmpeg.jpg -font Helvetica -antialias -gravity NorthWest `+
				`-fill '#aaaaaa' -pointsize 50 `+
				`-annotate +20+5 '%v' temp.jpg && mv temp.jpg output.jpg && popd`,
			ws.config.ImageDir, timeStr)
		cmd = exec.Command("bash", "-c", addTimeCmd)
		cmdOutput, innerErr := cmd.CombinedOutput()
		if innerErr != nil {
			slog.Error("failed to write time", "err", innerErr, "output", string(cmdOutput))
		}
	}()

	// Fetch weather data (cached for 10 minutes)
	nowSecs := ws.clock.Now().Unix()
	if ws.lastWeatherData == nil || nowSecs-ws.lastWeatherData.timestamp > 10*60 {
		var data map[string]any
		params := fmt.Sprintf("?zip=98112&appid=%v", ws.config.OpenWeatherAPIKey)
		url := "https://api.openweathermap.org/data/2.5/weather" + params
		if err = makeApiRequest(url, &data); err != nil {
			err = Wrap(err, "failed to get weather data")
			return
		}
		ws.lastWeatherData = &weatherData{
			owm:       data,
			timestamp: nowSecs,
		}
	}

	// Convert Kelvin to Fahrenheit
	data := ws.lastWeatherData.owm
	var temp, tempFeels, humidity, wind, windGust float64
	if data["main"] != nil {
		temp = getFloat(data["main"].(map[string]any)["temp"])
		temp = math.Round(1.8*(temp-273.0) + 32.0)
		tempFeels = getFloat(data["main"].(map[string]any)["feels_like"])
		tempFeels = math.Round(1.8*(tempFeels-273.0) + 32.0)
		humidity = getFloat(data["main"].(map[string]any)["humidity"])
	}

	// Convert m/s to mph
	if data["wind"] != nil {
		wind = getFloat(data["wind"].(map[string]any)["speed"])
		wind = math.Round(wind * 2.24)
		windGust = getFloat(data["wind"].(map[string]any)["gust"])
		windGust = math.Round(windGust * 2.24)
	}

	timeStr := ws.clock.NowPacific().Format("2006-01-02   3:04PM")
	lineHeight := 60
	imageMagickCmd := fmt.Sprintf(
		`pushd %s && `+
			`convert ffmpeg.jpg -font Helvetica -antialias -gravity NorthWest `+
			`-fill '#aaaaaa' -pointsize 50 `+
			`-annotate +20+5 '%v' `+
			`-annotate +20+%d 'Temp: %.0fF (feels-like %.0fF)' `+
			`-annotate +20+%d 'Wind: %.0f mph (Gusts: %.0f mph)' `+
			`-annotate +20+%d 'Humidity: %.0f%%' temp.jpg && mv temp.jpg output.jpg && popd`,
		ws.config.ImageDir, timeStr, (5 + lineHeight), temp, tempFeels, (5 + 2*lineHeight),
		wind, windGust, (5 + 3*lineHeight), humidity)
	cmd = exec.Command("bash", "-c", imageMagickCmd)
	cmdOutput, err = cmd.CombinedOutput()
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to update ffmpeg image")
		return
	}
	return nil
}

func makeApiRequest(url string, dst any) error {
	resp, err := http.Get(url)
	if err != nil {
		return Wrap(err, "http get failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Wrap(err, "reading response body")
	}
	if err = json.Unmarshal(body, dst); err != nil {
		return Wrap(err, "unmarshaling response")
	}
	return nil
}

func getFloat(val any) float64 {
	if val == nil {
		return 0.0
	}
	return val.(float64)
}
