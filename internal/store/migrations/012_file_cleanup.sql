CREATE TABLE file_cleanup (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 file_id INTEGER NOT NULL,
 folder TEXT NOT NULL,
 object_key TEXT NOT NULL,
 deleted_at INTEGER NOT NULL
);
CREATE INDEX file_cleanup_age ON file_cleanup(deleted_at,id);
INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at)
 SELECT CAST(entity_id AS INTEGER),json_extract(before_json,'$.folder'),json_extract(before_json,'$.object_key'),created_at
 FROM activity WHERE entity='file' AND action='delete' AND json_valid(before_json)
 AND json_extract(before_json,'$.folder') IS NOT NULL AND json_extract(before_json,'$.object_key') IS NOT NULL;
CREATE TRIGGER files_cleanup_delete BEFORE DELETE ON files BEGIN
 INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at)
 VALUES(old.id,old.folder,old.object_key,unixepoch());
END;
CREATE TRIGGER files_cleanup_replaced BEFORE UPDATE OF object_key ON files WHEN old.object_key<>new.object_key AND old.object_key<>'' BEGIN
 INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at)
 VALUES(old.id,old.folder,old.object_key,unixepoch());
END;
INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at)
 SELECT CAST(entity_id AS INTEGER),json_extract(before_json,'$.folder'),json_extract(before_json,'$.object_key'),created_at
 FROM activity WHERE entity='file' AND action='complete' AND json_valid(before_json) AND json_valid(after_json)
 AND json_extract(before_json,'$.object_key')<>json_extract(after_json,'$.object_key');
