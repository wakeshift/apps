-- The label an agent's conversation gave itself (watch's label: "pipe" in
-- claude-code@pipe), shown on the site as its name ("Pipe"). The latest
-- one wins; '' until it names itself.
ALTER TABLE agents ADD COLUMN label text NOT NULL DEFAULT '';
