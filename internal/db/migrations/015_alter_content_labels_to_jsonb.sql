-- +goose Up
-- Change content_labels from TEXT[] to JSONB to preserve full com.atproto.label.defs#selfLabels structure
-- This allows storing the optional 'neg' field and future extensions

-- Create temporary function to convert TEXT[] to selfLabels JSONB
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION convert_labels_to_jsonb(labels TEXT[])
RETURNS JSONB AS $$
BEGIN
  IF labels IS NULL OR array_length(labels, 1) = 0 THEN
    RETURN NULL;
  END IF;

  RETURN jsonb_build_object(
    'values',
    (SELECT jsonb_agg(jsonb_build_object('val', label))
     FROM unnest(labels) AS label)
  );
END;
$$ LANGUAGE plpgsql IMMUTABLE;
-- +goose StatementEnd

-- Convert column type using the function
ALTER TABLE posts
  ALTER COLUMN content_labels TYPE JSONB
  USING convert_labels_to_jsonb(content_labels);

-- Drop the temporary function
DROP FUNCTION convert_labels_to_jsonb(TEXT[]);

-- Update column comment
COMMENT ON COLUMN posts.content_labels IS 'Self-applied labels per com.atproto.label.defs#selfLabels (JSONB: {"values":[{"val":"nsfw","neg":false}]})';

-- +goose Down
-- Revert JSONB back to TEXT[] (lossy - drops 'neg' field)
ALTER TABLE posts
  ALTER COLUMN content_labels TYPE TEXT[]
  USING CASE
    WHEN content_labels IS NULL THEN NULL
    ELSE ARRAY(
      SELECT value->>'val'
      FROM jsonb_array_elements(content_labels->'values') AS value
    )
  END;

-- Restore original comment
COMMENT ON COLUMN posts.content_labels IS 'Self-applied labels (nsfw, spoiler, violence)';
