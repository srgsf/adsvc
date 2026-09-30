-- The latest ad.add of each origin per ad. A retraction counts only while it is newer than
-- the author's latest add of the ad, so an author who withdrew an ad can add it again (the
-- same audio is the same id). Derived from the records, like ads.
CREATE TABLE ad_adds (
	ad     BLOB NOT NULL,
	origin BLOB NOT NULL,
	ts     INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;

INSERT INTO ad_adds (ad, origin, ts) SELECT id, author, author_ts FROM ads WHERE author IS NOT NULL;
