import os
keywords=['john','password','secret','token','apikey','api_key','credentials']
for root, dirs, files in os.walk('.'):
    for fname in files:
        if fname.startswith('.git'): continue
        if fname.endswith(('.pyc', '.png', '.jpg', '.ico','.zip','.tar','.exe','.dll')): continue
        path=os.path.join(root,fname)
        try:
            data=open(path,'r',encoding='utf-8',errors='ignore').read().lower()
        except Exception:
            continue
        for kw in keywords:
            if kw in data:
                print(path, kw)
