-- Short strings exercise: embedded escapes that would confuse a regex.
local a = "hello\"world"
local b = 'it\'s a test'
local c = "with backslash: \\"
local d = "tab\there"
return a, b, c, d
