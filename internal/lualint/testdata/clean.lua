-- A clean file with nothing to flag, used to assert zero findings under
-- the empty-rule-set integration test.
local function add(a, b)
    return a + b
end

local function greet(name)
    return "hello, " .. name
end

return { add = add, greet = greet }
