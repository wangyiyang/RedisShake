FROM busybox

COPY ./bin/redis-shake /usr/local/app/redis-shake
COPY ./conf/redis-shake.conf /usr/local/app/redis-shake.conf
ENV TYPE sync
<<<<<<< HEAD
CMD /usr/local/app/redis-shake -type=${TYPE} -conf=/usr/local/app/redis-shake.conf
=======
CMD /usr/local/app/redis-shake -type=${TYPE} -conf=/usr/local/app/redis-shake.conf
>>>>>>> 4887a82a71e4bc20469834e46c5dbba46d6b0946
