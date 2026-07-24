package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	internalUtils "github.com/jiotv-go/jiotv_go/v3/internal/utils"
	"github.com/jiotv-go/jiotv_go/v3/pkg/secureurl"
	pkgUtils "github.com/jiotv-go/jiotv_go/v3/pkg/utils"
	"github.com/valyala/fasthttp"
)

const (
	catchupEPGURL   = "https://jiotvapi.cdn.jio.com/apis/v1.3/getepg/get?offset=%d&channel_id=%s&langId=%d"
	okhttpUserAgent = "okhttp/4.12.13"
	defaultLangID   = 6
	epochThreshold  = 100000000000
	catchupDays     = 7
)

func CatchupHandler(c *fiber.Ctx) error {
	id := c.Params("id")
	offsetStr := c.Query("offset", "0")
	offset, err := strconv.Atoi(offsetStr)
	if err != nil {
		offset = 0
		pkgUtils.Log.Printf("Invalid offset query parameter, defaulting to 0: %v", err)
	}

	epgData, err := getCatchupEPG(id, offset)
	if err != nil {
		pkgUtils.Log.Println("Error fetching catchup EPG:", err)
		return c.Render("views/catchup", fiber.Map{
			"Title":       Title,
			"Error":       "Could not fetch catchup data",
			"Channel":     id,
			"LivePlayURL": "/play/" + id + "?live=true",
		})
	}

	currentTime := time.Now().UnixMilli()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}

	var pastEpgData []map[string]interface{}
	for _, p := range epgData {
		if start, ok := p["startEpoch"].(int64); ok {
			if start < epochThreshold {
				start = start * 1000
			}
			if start > currentTime {
				continue
			}
			startTime := time.UnixMilli(start).In(loc)
			p["showtime"] = startTime.Format("03:04 PM")
			if end, ok := p["endEpoch"].(int64); ok {
				if end < epochThreshold {
					end = end * 1000
				}
				endTime := time.UnixMilli(end).In(loc)
				p["endtime"] = endTime.Format("03:04 PM")
				if start <= currentTime && end > currentTime {
					p["IsLive"] = true
				}
			}
		}
		pastEpgData = append(pastEpgData, p)
	}

	currentDate := time.Now().In(loc).AddDate(0, 0, offset).Format("02/01/2006")
	showNext := offset < 0
	showPrev := offset > -catchupDays

	return c.Render("views/catchup", fiber.Map{
		"Title":       Title,
		"Data":        pastEpgData,
		"Channel":     id,
		"Offset":      offset,
		"NextOffset":  offset + 1,
		"PrevOffset":  offset - 1,
		"CurrentDate": currentDate,
		"ShowNext":    showNext,
		"ShowPrev":    showPrev,
	})
}

func CatchupStreamHandler(c *fiber.Ctx) error {
	id := c.Params("id")
	start := c.Query("start")
	end := c.Query("end")

	if start == "" || end == "" {
		return fiber.NewError(fiber.StatusBadRequest, "Missing start or end time")
	}

	startMillis, startFormatted, err := normalizeCatchupTime(start)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid catchup start time")
	}
	endMillis, endFormatted, err := normalizeCatchupTime(end)
	if err != nil || endMillis <= startMillis {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid catchup end time")
	}

	srno := c.Query("srno")
	if srno == "" {
		srno, err = resolveCatchupSerial(id, startMillis, endMillis)
		if err != nil {
			pkgUtils.Log.Printf("Failed to resolve catchup programme for channel %s: %v", id, err)
			return fiber.NewError(fiber.StatusBadGateway, "Could not resolve catchup programme")
		}
	}

	if err := EnsureFreshTokens(); err != nil {
		pkgUtils.Log.Printf("Failed to ensure fresh tokens: %v", err)
	}

	pkgUtils.Log.Printf("Fetching catchup URL for channel %s, start: %s, end: %s, srno: %s", id, startFormatted, endFormatted, srno)
	catchupResult, err := TV.GetCatchupURL(id, srno, startFormatted, endFormatted)
	if err != nil {
		pkgUtils.Log.Printf("Error fetching catchup URL: %v", err)
		return internalUtils.InternalServerError(c, err)
	}

	targetURL := catchupResult.Bitrates.Auto
	if targetURL == "" {
		targetURL = catchupResult.Result
	}
	pkgUtils.Log.Printf("Catchup Target URL: %s", targetURL)

	if targetURL == "" {
		return internalUtils.InternalServerError(c, fmt.Errorf("failed to get catchup URL from API"))
	}

	codedUrl, err := secureurl.EncryptURL(targetURL)
	if err != nil {
		return internalUtils.InternalServerError(c, err)
	}

	redirectURL := fmt.Sprintf("/render.m3u8?auth=%s&channel_key_id=%s", codedUrl, id)
	// Ensure we don't double-append hdnea if it's already in the URL
	if catchupResult.Hdnea != "" && !strings.Contains(targetURL, "hdnea=") {
		redirectURL += "&hdnea=" + catchupResult.Hdnea
	}
	return c.Redirect(redirectURL, fiber.StatusFound)
}

func normalizeCatchupTime(value string) (int64, string, error) {
	for _, layout := range []string{"20060102T150405", "20060102150405"} {
		parsed, err := time.ParseInLocation(layout, value, time.UTC)
		if err == nil {
			return parsed.UnixMilli(), parsed.Format("20060102T150405"), nil
		}
	}

	if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
		if epoch < epochThreshold {
			epoch *= 1000
		}
		if epoch <= 0 {
			return 0, "", fmt.Errorf("invalid epoch %q", value)
		}
		parsed := time.UnixMilli(epoch).UTC()
		return epoch, parsed.Format("20060102T150405"), nil
	}
	return 0, "", fmt.Errorf("unsupported catchup time %q", value)
}

func resolveCatchupSerial(channelID string, startMillis, endMillis int64) (string, error) {
	offset := catchupDayOffset(startMillis, time.Now())
	if offset > 0 || offset < -catchupDays {
		return "", fmt.Errorf("programme offset %d is outside the catchup window", offset)
	}

	offsets := []int{offset}
	if offset > -catchupDays {
		offsets = append(offsets, offset-1)
	}
	if offset < 0 {
		offsets = append(offsets, offset+1)
	}

	for _, candidateOffset := range offsets {
		epgData, err := getCatchupEPG(channelID, candidateOffset)
		if err != nil {
			continue
		}
		if srno, ok := findCatchupSerial(epgData, startMillis, endMillis); ok {
			return srno, nil
		}
	}
	return "", fmt.Errorf("no matching programme found")
}

func catchupDayOffset(startMillis int64, now time.Time) int {
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		location = time.FixedZone("IST", 5*60*60+30*60)
	}
	start := time.UnixMilli(startMillis).In(location)
	current := now.In(location)
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
	currentDay := time.Date(current.Year(), current.Month(), current.Day(), 0, 0, 0, 0, location)
	return int(startDay.Sub(currentDay).Hours() / 24)
}

func findCatchupSerial(epgData []map[string]interface{}, startMillis, endMillis int64) (string, bool) {
	const tolerance = int64(90 * time.Second / time.Millisecond)
	for _, programme := range epgData {
		programmeStart, startOK := catchupInt64(programme["startEpoch"])
		programmeEnd, endOK := catchupInt64(programme["endEpoch"])
		if !startOK || !endOK {
			continue
		}
		if programmeStart < epochThreshold {
			programmeStart *= 1000
		}
		if programmeEnd < epochThreshold {
			programmeEnd *= 1000
		}
		if absoluteDifference(programmeStart, startMillis) > tolerance ||
			absoluteDifference(programmeEnd, endMillis) > tolerance {
			continue
		}
		if srno, ok := catchupString(programme["srno"]); ok && srno != "" {
			return srno, true
		}
	}
	return "", false
}

func catchupInt64(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func catchupString(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case int:
		return strconv.Itoa(typed), true
	case float64:
		return strconv.FormatInt(int64(typed), 10), true
	default:
		return "", false
	}
}

func absoluteDifference(left, right int64) int64 {
	if left > right {
		return left - right
	}
	return right - left
}

func CatchupPlayerHandler(c *fiber.Ctx) error {
	id := c.Params("id")
	start := c.Query("start")
	end := c.Query("end")
	srno := c.Query("srno")
	showName := c.Query("showname", "Catchup Show")
	description := c.Query("description", "No description available")
	episodePoster := c.Query("poster", "")
	showTime := c.Query("showtime", "")

	playerURL := fmt.Sprintf("/catchup/render/%s?start=%s&end=%s&srno=%s&v=6", id, start, end, srno)

	return c.Render("views/catchup_player", fiber.Map{
		"Title":         Title,
		"ChannelID":     id,
		"ShowName":      showName,
		"Description":   description,
		"EpisodePoster": episodePoster,
		"ShowTime":      showTime,
		"player_url":    playerURL,
	})
}

func CatchupRenderPlayerHandler(c *fiber.Ctx) error {
	id := c.Params("id")
	start := c.Query("start")
	end := c.Query("end")
	srno := c.Query("srno")
	quality := c.Query("q", "")
	qualityForDrm := quality
	if qualityForDrm == "" {
		qualityForDrm = "high"
	}

	playURL := fmt.Sprintf("/catchup/stream/%s?start=%s&end=%s&srno=%s", id, start, end, srno)
	if quality != "" {
		playURL += "&q=" + quality
	}

	startFmt := start
	endFmt := end
	startInt, errStart := strconv.ParseInt(start, 10, 64)
	endInt, errEnd := strconv.ParseInt(end, 10, 64)
	if errStart == nil && errEnd == nil {
		startFmt = time.UnixMilli(startInt).UTC().Format("20060102T150405")
		endFmt = time.UnixMilli(endInt).UTC().Format("20060102T150405")
	}

	if err := EnsureFreshTokens(); err != nil {
		pkgUtils.Log.Printf("Failed to ensure fresh tokens: %v", err)
	}

	catchupResult, err := TV.GetCatchupURL(id, srno, startFmt, endFmt)
	if err == nil && catchupResult != nil && catchupResult.IsDRM {
		mpdURL := internalUtils.SelectQuality(qualityForDrm, catchupResult.Mpd.Bitrates.Auto, catchupResult.Mpd.Bitrates.High, catchupResult.Mpd.Bitrates.Medium, catchupResult.Mpd.Bitrates.Low)
		if mpdURL == "" {
			candidates := []string{
				catchupResult.Mpd.Bitrates.High,
				catchupResult.Mpd.Bitrates.Auto,
				catchupResult.Mpd.Bitrates.Medium,
				catchupResult.Mpd.Bitrates.Low,
				catchupResult.Mpd.Result,
			}
			for _, candidate := range candidates {
				if candidate != "" {
					mpdURL = candidate
					break
				}
			}
		}

		if mpdURL != "" {
			encMpdUrl, encErr := secureurl.EncryptURL(mpdURL)
			if encErr == nil {
				licenseUrl := ""
				if catchupResult.Mpd.Key != "" {
					encKey, keyErr := secureurl.EncryptURL(catchupResult.Mpd.Key)
					if keyErr == nil {
						licenseUrl = "/drm?auth=" + encKey + "&channel_id=" + id + "&channel=" + encMpdUrl
					}
				}

				if catchupResult.AlgoName == "timesplay" {
					return c.Render("views/player_drm", fiber.Map{
						"play_url":     mpdURL,
						"license_url":  licenseUrl,
						"channel_host": "",
						"channel_path": "",
					})
				}

				parsedTvUrl, parseErr := url.Parse(mpdURL)
				if parseErr == nil {
					tvUrlSplit := strings.Split(parsedTvUrl.Path, "/")
					if len(tvUrlSplit) > 1 {
						tvUrlPath, pathErr := secureurl.EncryptURL(strings.Join(tvUrlSplit[:len(tvUrlSplit)-1], "/") + "/")
						tvUrlHost, hostErr := secureurl.EncryptURL(parsedTvUrl.Host)
						if pathErr == nil && hostErr == nil {
							return c.Render("views/player_drm", fiber.Map{
								"play_url":     "/render.mpd?auth=" + encMpdUrl,
								"license_url":  licenseUrl,
								"channel_host": tvUrlHost,
								"channel_path": tvUrlPath,
							})
						}
					}
				}
			}
		}
	}

	return c.Render("views/player_hls", fiber.Map{
		"play_url":   playURL,
		"is_catchup": true,
	})
}

func getCatchupEPG(id string, offset int) ([]map[string]interface{}, error) {
	url := fmt.Sprintf(catchupEPGURL, offset, id, defaultLangID)

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(url)
	req.Header.SetMethod("GET")
	req.Header.Set("Host", "jiotvapi.cdn.jio.com")
	req.Header.Set("user-agent", okhttpUserAgent)
	req.Header.Set("Accept-Encoding", "gzip")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	client := &fasthttp.Client{}
	if err := client.Do(req, resp); err != nil {
		return nil, err
	}

	var body []byte
	var err error

	contentEncoding := resp.Header.Peek("Content-Encoding")
	if bytes.Contains(contentEncoding, []byte("gzip")) {
		body, err = resp.BodyGunzip()
		if err != nil {
			return nil, err
		}
	} else {
		body = resp.Body()
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if epg, ok := result["epg"].([]interface{}); ok {
		epgList := make([]map[string]interface{}, len(epg))
		for i, v := range epg {
			if m, ok := v.(map[string]interface{}); ok {
				if start, ok := m["startEpoch"].(float64); ok {
					m["startEpoch"] = int64(start)
				}
				if end, ok := m["endEpoch"].(float64); ok {
					m["endEpoch"] = int64(end)
				}
				if srno, ok := m["srno"].(float64); ok {
					m["srno"] = fmt.Sprintf("%.0f", srno)
				}
				epgList[len(epg)-1-i] = m
			}
		}
		return epgList, nil
	}

	return nil, fmt.Errorf("epg field not found or not a list")
}
