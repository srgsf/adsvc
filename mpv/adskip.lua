-- adskip.lua: mpv client for adsvc. It skips known ads, and marks new ones.
-- Install: copy to the mpv scripts folder (~/.config/mpv/scripts/, on Windows
-- %APPDATA%\mpv\scripts\). The copy the adsvc page hands out carries your token already;
-- otherwise set it in script-opts/adskip.conf:
--   ads_token=<token>   (when adsvc has users: one of auth.users; also sent on the stream request)
--   ads_url=http://127.0.0.1:8080   (only for streams that do not go through adsvc's /s)
-- Play through adsvc: mpv 'http://HOST:8080/s?u=<source URL>[&ih=<infohash>&idx=1]'. The
-- Reports page of adsvc gives a command like that for every ad you reported; marking the
-- ad clears the report.
-- adsvc reports two types of segment, "ad" and "intro"; this client skips both.
-- Keys: , and . = a frame back / forward (the time is shown), a = ad start, t = intro
-- (title sequence) start, A = end of what was started (asks for a label, then enrolls it),
-- Ctrl+a = toggle auto-skip.
local utils = require 'mp.utils'
local opts = { ads_url = "", ads_token = "", poll = 2, auto_skip = true }
require('mp.options').read_options(opts, "adskip")
local input_ok, input = pcall(require, 'mp.input') -- mpv 0.39+

local base = nil  -- adsvc's URL for the current file
local query = nil -- query string identifying the current file for the adsvc API
local ads = {}
local mark_start = nil
local mark_type = "ad" -- what mark_start started: "ad" or "intro"
-- ads skipped once in this file, keyed by ad id and start. Seeking back into one plays it;
-- going back to before its start re-arms the skip.
local skipped = {}
local show_step = false

-- adsvc_base is the adsvc URL a stream goes through (the part before /s?, without user
-- info), or the ads_url option.
local function adsvc_base(path)
    local scheme, rest = (path or ""):match("^(https?://)([^/]+)/s%?")
    if scheme then return scheme .. rest:gsub("^.*@", "") end
    if opts.ads_url ~= "" then return opts.ads_url end
    return nil
end

local function file_query(path)
    if not path then return nil end
    -- through adsvc: /s?u=<upstream>[&id=<id>|&ih=<infohash>&idx=<n>]
    local u = path:match("[?&]u=([^&]+)")
    if u then
        local id = path:match("[?&]id=([^&]+)")
        if id then return "?id=" .. id end
        local ih = path:match("[?&]ih=([^&]+)")
        if ih then return "?ih=" .. ih .. "&idx=" .. (path:match("[?&]idx=([^&]+)") or "1") end
        return "?u=" .. u -- already escaped inside the URL; adsvc spots TorrServer URLs in it
    end
    return nil
end

-- ms is mpv's time in seconds as the integer milliseconds adsvc's API takes.
local function ms(sec)
    return string.format("%d", math.floor(sec * 1000 + 0.5))
end

local function urlencode(s)
    return (s:gsub("[^%w%-_%.~]", function(c) return string.format("%%%02X", c:byte()) end))
end

local function clock(t)
    t = t or 0
    return string.format("%d:%02d:%06.3f", math.floor(t / 3600), math.floor(t % 3600 / 60), t % 60)
end

-- api calls adsvc; cb gets (ok, body, status).
local function api(method, route, extra, cb)
    local url = base .. route .. (query or "") .. (extra or "")
    local args = { "curl", "-s", "-X", method, "-w", "\n%{http_code}", url }
    if opts.ads_token ~= "" then
        table.insert(args, "-H")
        table.insert(args, "Authorization: Bearer " .. opts.ads_token)
    end
    mp.command_native_async({ name = "subprocess", playback_only = false, capture_stdout = true,
        args = args }, function(ok, res)
        if not cb then return end
        local out = (ok and res and res.stdout) or ""
        local body, code = out:match("^(.*)\n(%d+)$")
        code = tonumber(code) or 0
        cb(code >= 200 and code < 300, body or "", code)
    end)
end

-- Polling: every opts.poll seconds while something may change; the interval doubles (up
-- to max_poll) while the answer stays the same, and goes back to opts.poll on a seek or a
-- change. Paused, nothing is polled.
local max_poll = 30
local interval = opts.poll
local timer = nil
local last_body = nil

local poll -- forward

local function schedule()
    if timer then timer:kill() end
    timer = nil
    if query and not mp.get_property_bool("pause") then
        timer = mp.add_timeout(interval, poll)
    end
end

-- reset polls again soon: something is likely to change (a seek, playback resumed).
local function reset()
    interval = opts.poll
    schedule()
end

poll = function()
    if not query then return end
    api("GET", "/ads/file", nil, function(ok, body)
        if ok and body == last_body then
            interval = math.min(interval * 2, max_poll) -- nothing new
        elseif ok then
            interval, last_body = opts.poll, body
            local r = utils.parse_json(body)
            if r and r.ads then
                ads = {}
                for _, a in ipairs(r.ads) do -- the API counts milliseconds, mpv seconds
                    local ad = { adId = a.adId, label = a.label, type = a.type or "ad", confirmed = a.confirmed,
                        start = a.startMs / 1000, ["end"] = a.endMs / 1000 }
                    ad.key = string.format("%s@%.1f", tostring(ad.adId), ad.start)
                    table.insert(ads, ad)
                end
            end
        end
        schedule()
    end)
end

-- the stream itself goes to adsvc too, so it needs the same token
mp.add_hook("on_load", 50, function()
    local path = mp.get_property("path") or ""
    local b = adsvc_base(path)
    if opts.ads_token ~= "" and b and path:gsub("//[^/]*@", "//"):sub(1, #b) == b then
        local h = mp.get_property_native("http-header-fields") or {}
        table.insert(h, "Authorization: Bearer " .. opts.ads_token)
        mp.set_property_native("file-local-options/http-header-fields", h)
    end
end)

mp.register_event("file-loaded", function()
    local path = mp.get_property("path")
    base, query = adsvc_base(path), file_query(path)
    if not base then query = nil end
    ads, skipped, mark_start, last_body, interval = {}, {}, nil, nil, opts.poll
    if timer then timer:kill() end
    timer = nil
    if not query then return end
    mp.msg.info("adskip: tracking " .. query)
    poll()
end)

mp.register_event("seek", reset)
mp.observe_property("pause", "bool", function(_, paused)
    if paused then
        if timer then timer:kill() end
        timer = nil
    elseif query then
        reset()
    end
end)

mp.observe_property("time-pos", "number", function(_, pos)
    if not pos then return end
    if show_step then
        show_step = false
        mp.osd_message(clock(pos) .. (mark_start and ("   start " .. clock(mark_start)) or ""), 3)
    end
    if not opts.auto_skip then return end
    for _, ad in ipairs(ads) do
        local k = ad.key
        if pos < ad.start then
            skipped[k] = nil
        elseif ad.confirmed and not skipped[k] and pos < ad["end"] - 0.5 then
            -- inside a confirmed ad: ran into it, seeked into it, or it was confirmed while playing
            skipped[k] = true
            mp.set_property_number("time-pos", ad["end"])
            mp.osd_message(string.format("Skipped %s: %s (%.0fs)", ad.type, ad.label, ad["end"] - pos), 3)
            break
        end
    end
end)

mp.add_forced_key_binding(".", "adskip-frame-step", function()
    show_step = true
    mp.command("frame-step")
end, { repeatable = true })

mp.add_forced_key_binding(",", "adskip-frame-back-step", function()
    show_step = true
    mp.command("frame-back-step")
end, { repeatable = true })

local function start_mark(kind)
    mark_start, mark_type = mp.get_property_number("time-pos"), kind
    mp.osd_message(string.format("%s start at %s, press A at the end", kind, clock(mark_start)), 3)
end
mp.add_key_binding("a", "adskip-mark-start", function() start_mark("ad") end)
mp.add_key_binding("t", "adskip-mark-intro", function() start_mark("intro") end)

local function enroll(start, stop, label, kind)
    local q = "&startMs=" .. ms(start) .. "&endMs=" .. ms(stop) .. "&type=" .. kind
    if label and label ~= "" then q = q .. "&label=" .. urlencode(label) end
    api("POST", "/ads/mark", q, function(ok, body, code)
        if not ok then
            mp.osd_message("Enroll failed (" .. code .. "): " .. body, 6)
            return
        end
        local ad = utils.parse_json(body) or {}
        local msg = kind .. " enrolled: " .. tostring(ad.label) .. string.format(" (%.1fs)", (ad.durationMs or 0) / 1000)
        mp.osd_message(msg, 5)
        reset() -- the file's ads just changed
    end)
end

mp.add_key_binding("A", "adskip-mark-end", function()
    local pos = mp.get_property_number("time-pos")
    if not (query and mark_start and pos and pos > mark_start) then
        mp.osd_message("Mark the start with 'a' (ad) or 't' (intro) first", 2)
        return
    end
    local start, kind = mark_start, mark_type
    mark_start = nil
    if not input_ok then -- older mpv: no prompt, adsvc names the ad
        enroll(start, pos, nil, kind)
        return
    end
    mp.set_property_bool("pause", true)
    input.get({
        prompt = string.format("%s %s-%s, label (Enter to save, Esc to cancel): ", kind, clock(start), clock(pos)),
        submit = function(label)
            input.terminate()
            enroll(start, pos, label, kind)
        end,
    })
end)

mp.add_key_binding("Ctrl+a", "adskip-toggle", function()
    opts.auto_skip = not opts.auto_skip
    mp.osd_message("Auto-skip of ads and intros: " .. (opts.auto_skip and "on" or "off"), 2)
end)
