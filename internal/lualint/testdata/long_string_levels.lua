-- Long strings with mismatched levels: the inner ]] does NOT close the
-- outer [==[…]==] block. A regex that looks for "[[" / "]]" naively
-- gets confused here.
local s = [==[
this contains ]] and [[ which would terminate a level-0 long string,
but not a level-2 one.
]==]
return s
