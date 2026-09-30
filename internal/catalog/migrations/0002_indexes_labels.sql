-- Indexes the admin page's and the ingest's queries use, and columns that keep per-row
-- work out of the list queries.

CREATE INDEX peer_aliases_map ON peer_aliases (key, origin); -- the cascade from peer_maps
CREATE INDEX dups_canonical ON dups (canonical);
CREATE INDEX ads_created ON ads (created DESC, id);            -- the Ads list's default order

-- The effective label (catalog.labelOf: this node's own label, else the latest label, else
-- the ad's) and its fold(), kept up to date by the catalogue (setLabels) for search and
-- sort. SetIdentity recomputes them with this node's origin at every start.
ALTER TABLE ads ADD COLUMN label_eff TEXT NOT NULL DEFAULT '';
ALTER TABLE ads ADD COLUMN label_fold TEXT NOT NULL DEFAULT '';
UPDATE ads SET label_eff = COALESCE((SELECT l.label FROM ad_labels l WHERE l.ad = ads.id
	ORDER BY l.ts DESC, l.origin DESC LIMIT 1), label);
UPDATE ads SET label_fold = fold(label_eff);
CREATE INDEX ads_label ON ads (label_fold, id);

-- The analysed milliseconds of a map: the sum of its analyzed ranges.
ALTER TABLE file_maps ADD COLUMN analyzed_ms INTEGER NOT NULL DEFAULT 0;
UPDATE file_maps SET analyzed_ms = (SELECT COALESCE(SUM(json_extract(value, '$[1]') - json_extract(value, '$[0]')), 0)
	FROM json_each(analyzed));
