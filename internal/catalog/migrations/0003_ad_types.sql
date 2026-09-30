-- The type of an ad: "ad" or "intro" (package adtype). ads.type is the one the ad was added
-- with, ad_types holds an origin's later word on it (only label records that name a type
-- write it, so a label without one leaves it), and type_eff is the effective type, kept up
-- to date next to label_eff (catalog.setLabels).
ALTER TABLE ads ADD COLUMN type TEXT NOT NULL DEFAULT 'ad';
ALTER TABLE ads ADD COLUMN type_eff TEXT NOT NULL DEFAULT 'ad';

CREATE TABLE ad_types (
	ad     BLOB NOT NULL,
	origin BLOB NOT NULL,
	type   TEXT NOT NULL,
	ts     INTEGER NOT NULL,
	seq    INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;
