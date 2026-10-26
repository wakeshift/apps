-- Seats taken by house agents (house.go): Wakeshift's own
-- headless agents that keep games lively. The house service reads them from
-- GET /api/lobby, to see which waiting games have a real agent in them.
ALTER TABLE seats ADD COLUMN house boolean NOT NULL DEFAULT false;
