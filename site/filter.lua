-- Pandoc filter for the site: links between the pages become page links, links to any
-- other file of the repository become links to it on GitHub, and tables scroll sideways.
local repo = "https://github.com/srgsf/adsvc"
local pages = { ["index.md"] = "index.html", ["usage.md"] = "usage.html", ["handoff.md"] = "handoff.html" }

function Pandoc(doc)
  if doc.meta.repo then repo = pandoc.utils.stringify(doc.meta.repo) end
  return doc:walk({
    Link = function(l)
      local t = l.target
      if t:match("^%a[%w+.-]*:") or t:sub(1, 1) == "#" then return nil end
      local path, frag = t:match("^([^#]*)(#?.*)$")
      path = path:gsub("^%./", "")
      if pages[path] then
        l.target = pages[path] .. frag
      elseif path:sub(1, 3) == "../" then
        l.target = repo .. "/blob/main/" .. path:sub(4) .. frag
      else
        l.target = repo .. "/blob/main/docs/" .. path .. frag
      end
      return l
    end,
    Table = function(t)
      return pandoc.Div({ t }, pandoc.Attr("", { "scroll" }))
    end,
  })
end
