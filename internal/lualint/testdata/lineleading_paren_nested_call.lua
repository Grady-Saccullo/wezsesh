-- Previous statement ends in `)` (a nested call). The line-leading
-- `(` is glued onto it as a function-call continuation: Lua parses
-- this as `f(g(x))(y)`. Regex linters that grep `^\s*\(` miss this
-- because the `)` is unbalanced relative to a naive paren-depth count.
local _ = f(g(x))
(y)
