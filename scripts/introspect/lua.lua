-- Dump the Lua standard library (string, table, math, io, os, ...) and the
-- global functions as lls JSON Lines.
-- Usage: lua scripts/introspect/lua.lua OUT.jsonl
local out = assert(io.open(arg[1] or "/dev/stdout", "w"))
local function q(s) return '"' .. tostring(s):gsub('[%c"\\]', function(c)
  return string.format("\\u%04x", c:byte()) end) .. '"' end
local function rec(m, k, n, d)
  out:write("{\"m\":", q(m), ",\"k\":", q(k), ",\"n\":", q(n), ",\"d\":", q(d), "}\n")
end
out:write('{"m":"_G"}\n')
for name, v in pairs(_G) do
  if type(name) == "string" and name:sub(1, 1) ~= "_" then
    if type(v) == "function" then rec("_G", "function", name, name .. "(...)")
    elseif type(v) == "table" and v ~= _G then
      rec("_G", "module", name, "module " .. name)
      out:write('{"m":', q(name), '}\n')
      for k, f in pairs(v) do
        if type(k) == "string" then
          if type(f) == "function" then rec(name, "function", k, name .. "." .. k .. "(...)")
          else rec(name, "constant", k, name .. "." .. k .. " = " .. tostring(f)) end
        end
      end
    else rec("_G", "variable", name, name .. ": " .. type(v)) end
  end
end
out:close()
