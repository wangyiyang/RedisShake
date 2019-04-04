import redis
import time
import uuid

r1=redis.Redis(host='127.0.0.1',port=6380,db=0)
r2=redis.Redis(host='127.0.0.1',port=6381,db=0)



if __name__ == '__main__':
	for i int range(100000000):
		key = str(uuid.uuid4())
		value = key