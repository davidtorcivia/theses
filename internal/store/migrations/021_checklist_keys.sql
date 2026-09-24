-- The production template spelled its checklist keys a0 to a3. A key ending in
-- zero is one frac refuses to place anything beside, so adding an item under a
-- lone a0 failed. 'a' is the same fraction in the spelling frac accepts; the
-- activity payloads move with the rows so undo still matches what they hold.
UPDATE checklist_items SET position = 'a' WHERE position = 'a0';
UPDATE activity SET before_json = json_set(before_json, '$.position', 'a')
 WHERE entity = 'checklist_item' AND json_extract(before_json, '$.position') = 'a0';
UPDATE activity SET after_json = json_set(after_json, '$.position', 'a')
 WHERE entity = 'checklist_item' AND json_extract(after_json, '$.position') = 'a0';
