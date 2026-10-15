-- godm-map: which parts of a download that is still arriving are already on
-- this computer, drawn over the film in mpv, so that it is plain how far the
-- film plays without waiting and where it would stop for data.
--
-- godm starts mpv with this script when it plays a download that has not
-- finished, and names the file to read in the script options:
--
--   --script-opts=godm-map-file=<file>,godm-map-lang=<zh|en>
--
-- godm rewrites that file about once a second while mpv streams the download:
-- the size of the file, the byte ranges on disk, the bytes its connections
-- are bringing in next, the download speed, and whether it is complete. The script
-- only ever reads it. It makes no network requests and knows no token. Once
-- nothing streams the download any more godm deletes the file, and the map
-- goes away.
--
-- The map shows while playback is paused or waiting for data, for a few
-- seconds after the mouse moves or a seek, and all the time while it is
-- switched on with h. In input.conf the switch is
-- "script-binding godm_map/toggle", and a binding of h there comes first.
--
-- The bar is the file in bytes, and the playhead is the byte mpv is playing,
-- which mpv says exactly while it reads the file the map is of (see look).
-- Time comes in only in the words: the bytes on disk ahead of the playhead
-- are taken to play at the film's average rate, its size over its duration.
-- A film's bitrate varies, so the time is approximate, good for how far there
-- is to go and not to the second. What mpv has already read ahead plays
-- without waiting whatever the map says, so how far that reaches counts too;
-- but once playback has stopped to wait for data, it is waiting, whatever
-- either says.

local mp = require "mp"
local utils = require "mp.utils"
local options = require "mp.options"

local opts = { file = "", lang = "en" }
options.read_options(opts, "godm-map")
if opts.file == "" then
    return -- not a download godm is streaming
end

local WORDS = {
    en = {
        ready = "Plays without waiting to %s (%s from here)",
        readyBytes = "%s downloaded ahead of here",
        toEnd = "Downloaded from here to the end",
        waiting = "This part is still downloading",
        speed = "Downloading at %s, the film needs %s",
        speedOnly = "Downloading at %s",
        slow = " (too slow)",
        complete = "Fully downloaded",
        paused = "The download is paused",
        queued = "The download is waiting in line",
        needs_refresh = "The download link expired",
        awaiting_refresh = "The download is waiting for a new link",
        error = "The download failed",
        stale = "godm has stopped updating this",
        on = "Download map: always shown",
        off = "Download map: shown when needed",
    },
    zh = {
        ready = "可连续播放到 %s（还剩 %s）",
        readyBytes = "前方已下载 %s",
        toEnd = "从这里到结尾都已下载",
        waiting = "这里还在下载",
        speed = "下载速度 %s，播放需要 %s",
        speedOnly = "下载速度 %s",
        slow = "（太慢）",
        complete = "已全部下载",
        paused = "下载已暂停",
        queued = "下载正在排队",
        needs_refresh = "下载链接已过期",
        awaiting_refresh = "下载正在等待新链接",
        error = "下载失败",
        stale = "godm 已停止更新",
        on = "下载地图：一直显示",
        off = "下载地图：需要时显示",
    },
}

-- How long the map stays up after the mouse moves or a seek, in seconds.
local LINGER = 3
-- How old what the file says may be, in milliseconds, before it is taken to be
-- no longer kept up to date: godm itself has stopped.
local STALE_MS = 10000

-- ASS colours are written blue, green, red.
local COLOUR = {
    missing = "202020", have = "71B43B", fetching = "FF8C5B", head = "FFFFFF",
    text = "FFFFFF", slow = "6B6BEE", faint = "B0B0B0",
}

local map = nil      -- what the file last said, of what could be read
local misses = 0     -- reads in a row that found no file to read
local toggled = false
local linger_until = 0
local shown = false
local overlay = mp.create_osd_overlay("ass-events")

local function lang()
    if map and WORDS[map.lang] then
        return map.lang
    end
    return WORDS[opts.lang] and opts.lang or "en"
end

local function clock(s)
    s = math.max(0, math.floor(s + 0.5))
    local h, m = math.floor(s / 3600), math.floor(s % 3600 / 60)
    if h > 0 then
        return string.format("%d:%02d:%02d", h, m, s % 60)
    end
    return string.format("%d:%02d", m, s % 60)
end

local function bytes(n)
    if n >= 1073741824 then
        return string.format("%.1f GB", n / 1073741824)
    elseif n >= 1048576 then
        return string.format("%.1f MB", n / 1048576)
    end
    return string.format("%.0f KB", n / 1024)
end

local function rate(n)
    return bytes(n) .. "/s"
end

-- read_map takes in what godm last wrote. A file that cannot be read whole
-- leaves the last good map in place: godm replaces the file in one step, but a
-- read that fails all the same must not blank the map for a second. A file
-- that has gone for a few reads in a row means godm has let go of the
-- download, and so does the map.
local function read_map()
    local f = io.open(opts.file, "rb")
    if not f then
        misses = misses + 1
        if misses >= 3 then
            map = nil
        end
        return
    end
    local text = f:read("*a")
    f:close()
    misses = 0
    local m = utils.parse_json(text or "")
    if type(m) == "table" and type(m.size) == "number" and m.size > 0 then
        map = m
    end
end

-- ranges is a list of [from, to) byte ranges as the file has them, leaving out
-- anything that is not one.
local function ranges(list)
    local out = {}
    if type(list) ~= "table" then
        return out
    end
    for _, r in ipairs(list) do
        if type(r) == "table" and type(r[1]) == "number" and type(r[2]) == "number" and r[2] > r[1] then
            out[#out + 1] = r
        end
    end
    return out
end

-- look works out where playback stands against the map.
local function look()
    local s = { known = map ~= nil, toggled = toggled, lang = lang() }
    -- A seek into a part that has not arrived does not make mpv pause for the
    -- cache: it stays "seeking", the picture frozen and nothing playing, for
    -- as long as the first bytes there take, which on a slow download is far
    -- longer than the few seconds a seek brings the map up for. That wait is
    -- when the map is wanted most.
    s.visible = toggled or mp.get_property_bool("pause", false) or
        mp.get_property_bool("paused-for-cache", false) or
        mp.get_property_bool("seeking", false) or mp.get_time() < linger_until
    if not map then
        s.visible = false
        return s, {}, {}
    end
    local size = map.size
    local have, fetching = ranges(map.have), ranges(map.fetching)
    s.ranges, s.fetching = #have, #fetching
    s.complete = map.complete == true
    s.speed = tonumber(map.speed) or 0
    s.state = map.state

    local dur = mp.get_property_number("duration")
    if dur and dur <= 0 then
        dur = nil
    end
    s.pos = mp.get_property_number("time-pos")
    -- stream-pos is where in the file the packet mpv last handed a decoder
    -- came from: the playhead in bytes, ahead of the picture only by what the
    -- decoders hold. It is taken while mpv reads the very file the map is of,
    -- and it is gone while a seek waits for its first packet. Then, or for
    -- another file, the playhead is put in proportion to the time, as
    -- percent-pos has it; percent-pos itself falls back to the byte position
    -- when a file has no duration. In proportion it can be a minute of film
    -- out, and land on bytes on disk while playback waits at their end.
    local sp, se = mp.get_property_number("stream-pos"), mp.get_property_number("stream-end")
    local pct = mp.get_property_number("percent-pos")
    if sp and se == size and sp >= 0 and sp <= size then
        s.playhead = math.floor(sp)
    else
        s.playhead = pct and math.floor(size * math.min(math.max(pct, 0), 100) / 100) or -1
    end
    s.need = dur and size / dur or -1

    -- The run of bytes on disk that the playhead is in, and where it ends.
    local run_end
    if s.playhead >= 0 then
        for _, r in ipairs(have) do
            if r[1] <= s.playhead and s.playhead < r[2] then
                run_end = r[2]
                break
            end
        end
    end
    s.to_end = s.complete or (run_end ~= nil and run_end >= size)
    s.ahead = run_end and run_end - s.playhead or 0
    s.map_ready = -1
    if s.to_end then
        s.map_ready = dur or -1
    elseif run_end and dur and s.pos then
        -- From where the film is, so that the guess of the rate is only made
        -- over the bytes ahead and not over all of the film before them.
        s.map_ready = s.pos + s.ahead / s.need
    end
    s.cached = mp.get_property_number("demuxer-cache-time") or -1
    s.stalled = mp.get_property_bool("paused-for-cache", false)
    s.ready_until = s.map_ready
    if s.stalled then
        -- Waiting for data: what mpv has buffered is all there is to go on.
        -- The map's run past the playhead, put in time at the average rate,
        -- can come to seconds where there are none, and to a minute of film
        -- with the playhead put in proportion.
        s.ready_until = s.cached
    elseif s.pos and s.cached > s.pos and s.cached > s.ready_until then
        s.ready_until = s.cached
    end
    s.slow = not s.complete and s.state == "running" and s.need > 0 and s.speed < s.need
    s.stale = type(map.at) == "number" and os.time() * 1000 - map.at > STALE_MS
    return s, have, fetching
end

-- say is the line of text over the map: how far playback runs from here, and
-- how the download is doing. The second part is nil once there is nothing
-- more to download.
local function say(s)
    local W = WORDS[s.lang]
    local first
    if s.complete then
        first = W.complete
    elseif s.to_end then
        first = W.toEnd
    elseif s.pos and s.ready_until > s.pos + 0.5 then
        first = string.format(W.ready, clock(s.ready_until), clock(s.ready_until - s.pos))
    elseif s.ahead > 0 and s.need <= 0 then
        -- No duration to put a time on it.
        first = string.format(W.readyBytes, bytes(s.ahead))
    else
        first = W.waiting
    end
    local second
    if not s.complete then
        if s.state == "running" then
            if s.need > 0 then
                second = string.format(W.speed, rate(s.speed), rate(s.need))
            else
                second = string.format(W.speedOnly, rate(s.speed))
            end
            if s.slow then
                second = second .. W.slow
            end
        else
            second = W[s.state]
        end
    end
    return first, second
end

-- subtitle_room is how far up from the bottom of the window, in 720ths of its
-- height, subtitles may reach, 0 while none are shown. Two lines of mpv's own
-- reach about 125 (sub-margin-y 34 and twice sub-font-size 38, with the space
-- between lines), and a Blu-ray's are drawn about as high; the map shows
-- while the film is paused, which is when a line is read, and must not lie
-- across it.
local function subtitle_room()
    if not mp.get_property_native("current-tracks/sub") or not mp.get_property_bool("sub-visibility", true) then
        return 0
    end
    local font = mp.get_property_number("sub-font-size", 38) * mp.get_property_number("sub-scale", 1)
    return math.max(150, mp.get_property_number("sub-margin-y", 34) + 2.5 * font + 12)
end

-- draw puts the map up, or takes it down when it is not to be seen. It is laid
-- out in the OSD's own pixels, so it is as sharp as the window, on the
-- 720-line scale the on-screen controller and subtitles use, so it keeps its
-- place above the controller's bar, and above the subtitles while there are
-- any, at any size of window.
local function draw(s, have, fetching, first, second)
    if not s.visible then
        if shown then
            overlay:remove()
            shown = false
        end
        return
    end
    local w, h = mp.get_osd_size()
    if not w or w <= 0 or not h or h <= 0 then
        w, h = 1280, 720 -- no window: lay it out all the same
    end
    local u = h / 720
    -- The default controller is a bar 56 units tall across the bottom.
    local osc = 56 * u
    if mp.get_property_native("osd-scale-by-window") == false then
        osc = 56
    end
    local side = math.floor(16 * u + 0.5)
    local thick = math.max(3, math.floor(7 * u + 0.5))
    local bottom = math.floor(h - math.max(osc + 14 * u, subtitle_room() * u) + 0.5)
    local top = bottom - thick
    local x0, x1 = side, math.floor(w) - side
    local span = x1 - x0
    local size = map.size

    local out = {}
    local function rect(colour, alpha, a, b, y0, y1)
        out[#out + 1] = string.format(
            "{\\an7\\pos(0,0)\\bord0\\shad0\\1c&H%s&\\1a&H%s&\\p1}m %d %d l %d %d %d %d %d %d{\\p0}",
            colour, alpha, a, y0, b, y0, b, y1, a, y1)
    end
    -- Ranges that fall on the same pixels are drawn as one, and the smallest
    -- is a pixel wide, so a map of many pieces stays a few dozen shapes and
    -- no piece goes unseen.
    local function runs(list, colour)
        local a, b
        for _, r in ipairs(list) do
            local ra = x0 + math.floor(span * r[1] / size)
            local rb = math.max(ra + 1, x0 + math.ceil(span * math.min(r[2], size) / size))
            if b and ra <= b then
                if rb > b then
                    b = rb
                end
            else
                if a then
                    rect(colour, "00", a, b, top, bottom)
                end
                a, b = ra, rb
            end
        end
        if a then
            rect(colour, "00", a, b, top, bottom)
        end
    end
    rect(COLOUR.missing, "50", x0, x1, top, bottom)
    runs(have, COLOUR.have)
    runs(fetching, COLOUR.fetching)
    if s.playhead >= 0 then
        local x = x0 + math.floor(span * s.playhead / size + 0.5)
        local half = math.max(1, math.floor(1.5 * u + 0.5))
        local over = math.floor(4 * u + 0.5)
        rect(COLOUR.head, "00", x - half, x + half, top - over, bottom + over)
    end

    local text = first
    if second then
        local colour = s.slow and COLOUR.slow or COLOUR.text
        text = text .. "   {\\1c&H" .. colour .. "&}" .. second
    end
    if s.stale then
        text = text .. "   {\\1c&H" .. COLOUR.faint .. "&}" .. WORDS[s.lang].stale
    end
    out[#out + 1] = string.format(
        "{\\an1\\pos(%d,%d)\\fs%d\\bord%.1f\\shad0\\1c&H%s&\\3c&H000000&}%s",
        x0, math.floor(top - 6 * u + 0.5), math.max(10, math.floor(20 * u + 0.5)), 1.6 * u, COLOUR.text, text)

    overlay.res_x, overlay.res_y = math.floor(w), math.floor(h)
    overlay.data = table.concat(out, "\n")
    overlay:update()
    shown = true
    s.shapes, s.bottom = #out, bottom
end

-- update looks again, draws, and publishes what it found in
-- user-data/godm-map, where a test or another script can read it.
local function update()
    local s, have, fetching = look()
    local first, second
    if s.known then
        first, second = say(s)
    end
    draw(s, have, fetching, first, second)
    mp.set_property_native("user-data/godm-map", {
        known = s.known,
        visible = s.visible,
        toggled = s.toggled,
        complete = s.complete or false,
        ranges = s.ranges or 0,
        fetching = s.fetching or 0,
        playhead = s.playhead or -1,
        map_ready = s.map_ready or -1,
        ready_until = s.ready_until or -1,
        need = s.need or -1,
        speed = s.speed or 0,
        slow = s.slow or false,
        stale = s.stale or false,
        stalled = s.stalled or false,
        lang = s.lang,
        shapes = s.shapes or 0,
        bottom = s.bottom or -1,
        text = first and (second and first .. " | " .. second or first) or "",
    })
end

-- linger shows the map for a while, after the mouse moved or a seek.
local function linger()
    linger_until = mp.get_time() + LINGER
    if not shown then
        update()
    end
end

read_map()
mp.add_periodic_timer(1, read_map)
mp.add_periodic_timer(0.5, update)
mp.observe_property("pause", "bool", update)
mp.observe_property("paused-for-cache", "bool", update)
mp.observe_property("seeking", "bool", update)
mp.observe_property("osd-dimensions", "native", function()
    if shown then
        update()
    end
end)
mp.observe_property("mouse-pos", "native", function(_, p)
    if type(p) == "table" and p.hover then
        linger()
    end
end)
mp.register_event("seek", linger)
mp.add_key_binding("h", "toggle", function()
    toggled = not toggled
    mp.osd_message(toggled and WORDS[lang()].on or WORDS[lang()].off, 2)
    update()
end)
