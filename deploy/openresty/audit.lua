local cjson = require "cjson.safe"

local audit_paths = {
  ["/v1/chat/completions"] = true,
  ["/v1/completions"] = true,
  ["/v1/responses"] = true,
  ["/v1/messages"] = true,
  ["/anthropic/v1/messages"] = true,
  ["/api/anthropic/v1/messages"] = true,
  ["/v1/embeddings"] = true,
  ["/v1/images/generations"] = true,
  ["/v1/images/edits"] = true,
  ["/v1/audio/transcriptions"] = true,
  ["/v1/audio/translations"] = true,
}

local function redact_headers(headers)
  local result = {}

  for k, v in pairs(headers or {}) do
    local key = string.lower(k)
    if key == "authorization"
      or key == "x-api-key"
      or key == "x-goog-api-key"
      or key == "cookie" then
      result[k] = v
    else
      result[k] = v
    end
  end

  return result
end

local function read_body()
  ngx.req.read_body()

  local data = ngx.req.get_body_data()
  if data then
    return data
  end

  local file_path = ngx.req.get_body_file()
  if not file_path then
    return nil
  end

  local file = io.open(file_path, "rb")
  if not file then
    return "[FAILED_TO_READ_TEMP_BODY_FILE]"
  end

  local content = file:read("*a")
  file:close()

  return content
end

local function write_audit_log(record)
  local line = cjson.encode(record)
  if not line then
    ngx.log(ngx.ERR, "failed to encode audit record")
    return
  end

  local file, err = io.open("/var/log/audit/request-body.jsonl", "a")
  if not file then
    ngx.log(ngx.ERR, "failed to open audit log: ", err)
    return
  end

  file:write(line)
  file:write("\n")
  file:close()
end

local uri = ngx.var.uri
if not audit_paths[uri] then
  return
end

local body = read_body()
local parsed_body = nil

if body and body ~= "" then
  parsed_body = cjson.decode(body)
end

local now = ngx.now()
local record = {
  time = now,
  time_local = ngx.localtime(),
  request_id = ngx.var.request_id,
  method = ngx.req.get_method(),
  uri = ngx.var.request_uri,
  path = uri,
  client_ip = ngx.var.remote_addr,
  user_agent = ngx.var.http_user_agent,
  headers = redact_headers(ngx.req.get_headers()),
  body = parsed_body or body,
}

write_audit_log(record)
