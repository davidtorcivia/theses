-- An API token and an MCP client are not actors of their own: they are a way
-- for a person to act. The activity row records that person, and via says what
-- they came through, empty for a browser session, "token:<name>" for the API
-- and "mcp:<client>" for MCP. The audit view reads it; everything else shows
-- the person's initials as it does for any other edit.
--
-- The number is 003 rather than 002, which 002_board_positions.sql already
-- took: two files at the same number would be applied in whichever order their
-- names happened to sort in.

ALTER TABLE activity ADD COLUMN via TEXT;
