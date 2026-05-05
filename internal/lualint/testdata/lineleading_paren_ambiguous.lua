-- §9.0.1.1 ambiguity: the (loadfile…)() on line 3 chains onto setup({…}).
local wezsesh = require("wezsesh")
wezsesh.setup({ binary = "/usr/local/bin/wezsesh" })
(loadfile("/tmp/probe.lua"))()
