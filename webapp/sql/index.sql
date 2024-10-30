alter table icons add index idx_user_id(user_id);
alter table livestream_tags add index idx_livestream_id(livestream_id);
alter table livecomments add index idx_livestream_id(livestream_id);
alter table livestreams add index idx_user_id(user_id);
alter table reactions add index idx_livestream_id(livestream_id);
alter table themes add index idx_user_id(user_id);
alter table ng_words add index idx_user_id_livestream_id(user_id, livestream_id);
