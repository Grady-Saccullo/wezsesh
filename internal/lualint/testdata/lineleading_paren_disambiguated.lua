-- Same shape as lineleading_paren_ambiguous.lua but with a leading `;`
-- that breaks the chain — this MUST NOT be flagged.
local wezsesh = require("wezsesh")
wezsesh.setup({ binary = "/usr/local/bin/wezsesh" });
(loadfile("/tmp/probe.lua"))()
