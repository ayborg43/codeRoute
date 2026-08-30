-- Stops every model already on file from looking newly released.
--
-- 010 added first_seen with a default of NOW(), which applied to every row
-- already there. The effect was that the first refresh after upgrading
-- reported the entire catalogue as new — hundreds of models — burying any
-- genuine arrival.
--
-- There is no record of when those models were really first seen, and
-- inventing one would be worse than admitting it: they are dated to the
-- distant past so they read as "already known", and history starts properly
-- from here. This migration runs immediately after 010, before the server
-- accepts traffic, so every row it touches is one that predates the tracking.

UPDATE discovered_models SET first_seen = TIMESTAMPTZ '2000-01-01 00:00:00Z';
