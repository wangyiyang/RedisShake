#!/bin/bash

sed -i "s/source.address =.*/source.address = {source_address}/g" conf/redis-shake.conf
sed -i "s/source.password_raw =.*/source.password_raw ={source_password}/g" conf/redis-shake.conf
sed -i "s/target.address =.*/target.address = {target_address}/g" conf/redis-shake.conf
sed -i "s/target.password_raw =.*/target.password_raw = {target_password}/g" conf/redis-shake.conf
