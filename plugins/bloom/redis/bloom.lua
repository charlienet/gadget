#!lua name=charlie_bloom

local function set_bit(keys, args) 
    for _, offset in ipairs(args) do
        redis.call("setbit", keys[1], offset, 1)
    end
end

local function test_bit(keys, args)
    for _, offset in ipairs(args) do
		if tonumber(redis.call("getbit", keys[1], offset)) == 0 then
			return false
		end
	end
	return true
end

redis.register_function('set_bit',set_bit)
redis.register_function('test_bit',test_bit)