-- The orphan report and the cleanup ask whether any file still holds a key,
-- or any key under a folder, once per deleted file.
CREATE INDEX files_object_key ON files(object_key);
