-- Previous statement ends in a short-string literal whose method is
-- about to be called by the line-leading `(` on the next line. Lua
-- parses this as `("foo"):upper()` then `("bar")`. The point of the
-- fixture is that a TokString counts as expression-end so the
-- ambiguity flag fires.
local _ = ("foo"):upper()
("bar")
