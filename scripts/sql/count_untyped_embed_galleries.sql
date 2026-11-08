-- Read-only census of stored imagesWithAttachment embeds whose gallery omits
-- its $type key. imagesWithAttachment.images is a plain ref, so $type there is
-- optional; migration 078 stamps it. A gallery with "$type": null has the key
-- and is not counted.
SELECT
    (SELECT count(*) FROM posts
      WHERE embed->>'$type' = 'social.coves.embed.imagesWithAttachment'
        AND jsonb_typeof(embed->'images') = 'object'
        AND NOT (embed->'images') ? '$type') AS posts_untyped,
    (SELECT count(*) FROM comments
      WHERE embed->>'$type' = 'social.coves.embed.imagesWithAttachment'
        AND jsonb_typeof(embed->'images') = 'object'
        AND NOT (embed->'images') ? '$type') AS comments_untyped;
