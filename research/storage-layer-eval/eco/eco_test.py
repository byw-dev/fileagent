"""Python data-ecosystem compatibility test against an S3 endpoint.

Every read/write after setup uses STS credentials narrowed by a session policy
to `raw/` (read-only) and `runs/r1/` (read-write), the way the CP would vend
them to an ETL run. Usage: eco_test.py <target> <host:port> <role_arn>
"""
import collections
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import time
import traceback

import boto3
import duckdb
import numpy as np
import pandas as pd
import pyarrow as pa
import pyarrow.dataset as pds
import pyarrow.fs as pafs
import pyarrow.parquet as pq
import s3fs
import xarray as xr

TARGET, EP, ROLE = sys.argv[1], sys.argv[2], sys.argv[3]
URL = "http://" + EP
B, RUN, RAW = "eco", "runs/r1/", "raw/"
HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(HERE, "work-" + TARGET)
os.makedirs(WORK, exist_ok=True)
os.makedirs(os.path.join(HERE, "..", "tmp"), exist_ok=True)
RESULTS = []

POLICY = {
    "Version": "2012-10-17",
    "Statement": [
        {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": [f"arn:aws:s3:::{B}"],
         "Condition": {"StringLike": {"s3:prefix": [RUN, RUN + "*", RAW, RAW + "*"]}}},
        {"Effect": "Allow", "Action": ["s3:GetBucketLocation", "s3:ListBucketMultipartUploads"],
         "Resource": [f"arn:aws:s3:::{B}"]},
        {"Effect": "Allow", "Action": ["s3:GetObject"], "Resource": [f"arn:aws:s3:::{B}/{RAW}*"]},
        {"Effect": "Allow",
         "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload",
                    "s3:ListMultipartUploadParts"],
         "Resource": [f"arn:aws:s3:::{B}/{RUN}*"]},
    ],
}


def rec(tid, name, status, detail=""):
    RESULTS.append({"id": tid, "name": name, "status": status, "detail": detail})
    print(f"{tid:5} {status:5} {name} — {str(detail)[:200]}", flush=True)


def step(tid, name):
    """Decorator: run fn, PASS with its return detail, FAIL on exception."""
    def wrap(fn):
        t0 = time.time()
        try:
            d = fn()
            rec(tid, name, "PASS", f"{d} [{time.time() - t0:.2f}s]" if d is not None else f"[{time.time() - t0:.2f}s]")
        except Exception as e:  # noqa: BLE001 - test harness records everything
            msg = f"{type(e).__name__}: {e}".replace("\n", " ")
            rec(tid, name, "FAIL", msg[:400])
            with open(os.path.join(WORK, "tracebacks.log"), "a") as f:
                f.write(f"==== {tid} {name}\n{traceback.format_exc()}\n")
        return fn
    return wrap


def expect_denied(fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        s = str(e)
        if any(k in s for k in ("AccessDenied", "ACCESS_DENIED", "Forbidden", "403", "PermissionError", "Access Denied")):
            return "denied as expected"
        raise AssertionError(f"failed, but not as access denial: {type(e).__name__}: {s[:200]}")
    raise AssertionError("request SUCCEEDED but should have been denied")


# ------------------------------------------------------------------ fixtures
admin_fs = s3fs.S3FileSystem(key="minioadmin", secret="minioadmin", client_kwargs={"endpoint_url": URL})


def make_nc(day):
    t = pd.date_range(f"2026-01-{day:02d}", periods=24, freq="h")
    rng = np.random.default_rng(day)
    lat, lon = np.linspace(-90, 90, 181), np.linspace(0, 359, 360)
    return xr.Dataset(
        {"precip": (("time", "lat", "lon"), rng.gamma(0.5, 2, (24, 181, 360)).astype("float32")),
         "temp": (("time", "lat", "lon"), (15 + 10 * rng.standard_normal((24, 181, 360))).astype("float32"))},
        coords={"time": t, "lat": lat, "lon": lon}, attrs={"source": "fileagent-eco-test"})


ENC = {v: {"zlib": True, "complevel": 4, "chunksizes": (1, 181, 360)} for v in ("precip", "temp")}
truth = {}
for day in (1, 2, 3):
    ds = make_nc(day)
    p = os.path.join(WORK, f"raw_{day}.nc")
    ds.to_netcdf(p, engine="h5netcdf", encoding=ENC)
    admin_fs.put(p, f"{B}/{RAW}nc/2026010{day}.nc")
    truth[day] = ds
rng = np.random.default_rng(42)
N = 1_000_000
obs = pd.DataFrame({
    "station": rng.integers(0, 200, N).astype("int32"),
    "ts": pd.date_range("2026-01-01", periods=N, freq="s"),
    "value": rng.normal(10, 3, N).round(3),
    "qc": rng.choice(["ok", "suspect", "bad"], N, p=[0.9, 0.08, 0.02]),
})
obs_csv = os.path.join(WORK, "obs.csv")
obs.to_csv(obs_csv, index=False)
admin_fs.put(obs_csv, f"{B}/{RAW}obs.csv")
print(f"fixtures: 3 NetCDF ({os.path.getsize(os.path.join(WORK, 'raw_1.nc')) / 2**20:.1f} MB each), "
      f"obs.csv {os.path.getsize(obs_csv) / 2**20:.1f} MB, {N:,} rows", flush=True)

# ------------------------------------------------------------------ STS (prefix-scoped)
sts = boto3.client("sts", endpoint_url=URL, aws_access_key_id="etladmin",
                   aws_secret_access_key="etladmin-secret", region_name="us-east-1")
C = {}


@step("A1", "AssumeRole with prefix-scoped session policy (raw/ ro, runs/r1/ rw)")
def _():
    c = sts.assume_role(RoleArn=ROLE, RoleSessionName="etl-r1", Policy=json.dumps(POLICY),
                        DurationSeconds=3600)["Credentials"]
    C.update(key=c["AccessKeyId"], secret=c["SecretAccessKey"], token=c["SessionToken"], exp=c["Expiration"])
    return f"expires {c['Expiration']}"


if not C:
    rec("A0", "abort", "FAIL", "no STS credentials; remaining tests skipped")
    json.dump(RESULTS, open(os.path.join(HERE, "..", "tmp", f"eco-{TARGET}.json"), "w"), indent=1, default=str)
    sys.exit(1)

s3 = boto3.client("s3", endpoint_url=URL, aws_access_key_id=C["key"], aws_secret_access_key=C["secret"],
                  aws_session_token=C["token"], region_name="us-east-1")
SO = {"key": C["key"], "secret": C["secret"], "token": C["token"], "client_kwargs": {"endpoint_url": URL}}
fs = s3fs.S3FileSystem(**SO)


@step("A2", "boto3 put_object (default checksums) inside runs/r1/")
def _():
    s3.put_object(Bucket=B, Key=RUN + "hello.txt", Body=b"hello")
    return "ok"


@step("A3", "write outside the run prefix is denied")
def _():
    return expect_denied(lambda: s3.put_object(Bucket=B, Key="runs/r2/x.txt", Body=b"x"))


@step("A4", "write into raw/ (read-only) is denied")
def _():
    return expect_denied(lambda: s3.put_object(Bucket=B, Key=RAW + "x.txt", Body=b"x"))


@step("A5", "list bucket root (outside allowed prefixes) is denied")
def _():
    return expect_denied(lambda: s3.list_objects_v2(Bucket=B, Prefix="", Delimiter="/"))


@step("A6", "list raw/ and runs/r1/ allowed")
def _():
    a = s3.list_objects_v2(Bucket=B, Prefix=RAW)["KeyCount"]
    b = s3.list_objects_v2(Bucket=B, Prefix=RUN)["KeyCount"]
    return f"raw/ {a} keys, runs/r1/ {b} keys"


@step("A7", "boto3 upload_fileobj of a NON-seekable 40MB stream (aws-chunked, trailing checksum)")
def _():
    class Stream:
        def __init__(self, data): self.data, self.pos = data, 0
        def read(self, n=-1):
            n = len(self.data) - self.pos if n < 0 else n
            b = self.data[self.pos:self.pos + n]; self.pos += len(b); return b
    data = os.urandom(40 * 2**20)
    s3.upload_fileobj(Stream(data), B, RUN + "stream.bin")
    got = s3.get_object(Bucket=B, Key=RUN + "stream.bin")["Body"].read()
    assert got == data, "content mismatch"
    return "40MB round-trip ok"


# ------------------------------------------------------------------ xarray / NetCDF
calls = collections.Counter()
_orig_call = s3fs.S3FileSystem._call_s3


async def _counted(self, method, *a, **k):
    calls[getattr(method, "__name__", str(method))] += 1
    return await _orig_call(self, method, *a, **k)


s3fs.S3FileSystem._call_s3 = _counted


@step("X1", "xarray lazy open of raw NetCDF (h5netcdf over s3fs) + one-timestep slice")
def _():
    calls.clear()
    with fs.open(f"{B}/{RAW}nc/20260101.nc", "rb") as f:
        ds = xr.open_dataset(f, engine="h5netcdf", chunks={})
        sl = ds["precip"].isel(time=5).sel(lat=slice(20, 50), lon=slice(100, 140)).values
    np.testing.assert_array_equal(sl, truth[1]["precip"].isel(time=5).sel(lat=slice(20, 50), lon=slice(100, 140)).values)
    return f"values match; S3 calls {dict(calls)}"


@step("X1b", "xarray lazy slice with 1MB s3fs blocks (forces HTTP Range reads)")
def _():
    calls.clear()
    with fs.open(f"{B}/{RAW}nc/20260101.nc", "rb", block_size=2**20, cache_type="readahead") as f:
        ds = xr.open_dataset(f, engine="h5netcdf", chunks={})
        sl = ds["temp"].isel(time=17).sel(lat=slice(-10, 10)).values
    np.testing.assert_array_equal(sl, truth[1]["temp"].isel(time=17).sel(lat=slice(-10, 10)).values)
    return f"values match; S3 calls {dict(calls)}"


@step("X2", "xarray direct URL: open_dataset('s3://…', storage_options=…)")
def _():
    ds = xr.open_dataset(f"s3://{B}/{RAW}nc/20260102.nc", engine="h5netcdf",
                         storage_options=SO, chunks={})
    v = float(ds["temp"].isel(time=0).mean())
    assert abs(v - float(truth[2]["temp"].isel(time=0).mean())) < 1e-4
    return f"mean ok ({v:.4f})"


@step("X3", "xarray open_mfdataset over glob s3://eco/raw/nc/*.nc + regional mean")
def _():
    calls.clear()
    files = ["s3://" + p for p in fs.glob(f"{B}/{RAW}nc/*.nc")]
    ds = xr.open_mfdataset([fs.open(p) for p in files], engine="h5netcdf", combine="by_coords", chunks={})
    m = float(ds["precip"].sel(lat=slice(0, 30), lon=slice(60, 120)).mean().compute())
    t = float(xr.concat([truth[d] for d in (1, 2, 3)], "time")["precip"].sel(lat=slice(0, 30), lon=slice(60, 120)).mean())
    assert abs(m - t) < 1e-4, (m, t)
    return f"{len(files)} files, {ds.sizes['time']} steps, mean ok; S3 calls {dict(calls)}"


out_ds = make_nc(9)


@step("X4", "[ecosystem limit] h5netcdf cannot write into a sequential S3 stream (needs seek)")
def _():
    try:
        with fs.open(f"{B}/{RUN}nc/direct.nc", "wb") as f:
            out_ds.to_netcdf(f, engine="h5netcdf", encoding=ENC)
    except OSError as e:
        return f"fails as expected on any S3 store: {e}"
    raise AssertionError("unexpectedly succeeded")


@step("X5", "xarray to_netcdf to local temp file + s3fs put (multipart) — the SDK helper path")
def _():
    p = os.path.join(WORK, "out.nc")
    out_ds.to_netcdf(p, engine="h5netcdf", encoding=ENC)
    fs.put(p, f"{B}/{RUN}nc/out.nc")
    with fs.open(f"{B}/{RUN}nc/out.nc", "rb") as f:
        xr.testing.assert_identical(xr.open_dataset(f, engine="h5netcdf").load(), out_ds)
    return f"{os.path.getsize(p) / 2**20:.1f} MB, identical"


@step("X6", "xarray to_netcdf(engine=scipy) to bytes + pipe (NetCDF3)")
def _():
    small = out_ds.isel(time=slice(0, 2))
    fs.pipe(f"{B}/{RUN}nc/nc3.nc", bytes(small.to_netcdf(engine="scipy")))
    with fs.open(f"{B}/{RUN}nc/nc3.nc", "rb") as f:
        xr.testing.assert_allclose(xr.open_dataset(f, engine="scipy").load(), small)
    return "ok"


# ------------------------------------------------------------------ pandas / pyarrow
@step("P1", "pandas read_csv raw/obs.csv (1M rows) via s3fs")
def _():
    df = pd.read_csv(f"s3://{B}/{RAW}obs.csv", storage_options=SO, parse_dates=["ts"])
    pd.testing.assert_frame_equal(df, obs, check_dtype=False)
    return f"{len(df):,} rows equal"


@step("P2", "pandas to_csv / read_csv round-trip in runs/r1/")
def _():
    obs.head(200_000).to_csv(f"s3://{B}/{RUN}pandas/obs.csv", index=False, storage_options=SO)
    df = pd.read_csv(f"s3://{B}/{RUN}pandas/obs.csv", storage_options=SO, parse_dates=["ts"])
    pd.testing.assert_frame_equal(df, obs.head(200_000), check_dtype=False)
    return "equal"


@step("P3", "pandas to_parquet / read_parquet round-trip (pyarrow engine via s3fs)")
def _():
    obs.to_parquet(f"s3://{B}/{RUN}pandas/obs.parquet", storage_options=SO)
    df = pd.read_parquet(f"s3://{B}/{RUN}pandas/obs.parquet", storage_options=SO)
    pd.testing.assert_frame_equal(df, obs, check_dtype=False)
    return "equal"


pfs = pafs.S3FileSystem(access_key=C["key"], secret_key=C["secret"], session_token=C["token"],
                        endpoint_override=EP, scheme="http", region="us-east-1")
big = pa.table({
    "station": pa.array(np.tile(np.arange(200, dtype="int32"), 30_000)),
    "day": pa.array(np.repeat(pd.date_range("2026-01-01", periods=30).strftime("%Y-%m-%d").to_numpy(), 200_000)),
    "value": pa.array(np.random.default_rng(1).normal(size=6_000_000)),
})


@step("P4", "pyarrow native S3FileSystem write_table 6M rows (multipart) + read back")
def _():
    pq.write_table(big, f"{B}/{RUN}arrow/big.parquet", filesystem=pfs)
    t = pq.read_table(f"{B}/{RUN}arrow/big.parquet", filesystem=pfs)
    assert t.equals(big), "table mismatch"
    size = pfs.get_file_info(f"{B}/{RUN}arrow/big.parquet").size
    return f"{size / 2**20:.1f} MB equal"


@step("P5a", "[ecosystem limit] write_dataset default HeadBucket needs bucket-wide ListBucket")
def _():
    return expect_denied(lambda: pds.write_dataset(big.slice(0, 10), f"{B}/{RUN}arrow/probe", filesystem=pfs,
                                                   format="parquet", existing_data_behavior="overwrite_or_ignore"))


@step("P5", "pyarrow write_dataset(create_dir=False) hive-partitioned by day (30 partitions) + filtered scan")
def _():
    pds.write_dataset(big, f"{B}/{RUN}arrow/ds", filesystem=pfs, format="parquet",
                      partitioning=pds.partitioning(pa.schema([("day", pa.string())]), flavor="hive"),
                      existing_data_behavior="overwrite_or_ignore", create_dir=False)
    d = pds.dataset(f"{B}/{RUN}arrow/ds", filesystem=pfs, format="parquet", partitioning="hive")
    t = d.to_table(filter=(pds.field("day") == "2026-01-07") & (pds.field("station") < 10))
    assert t.num_rows == 10 * 1000, t.num_rows
    return f"{len(d.files)} files discovered, filtered rows {t.num_rows}"


@step("P6", "pyarrow csv read of raw/obs.csv via native S3FileSystem")
def _():
    import pyarrow.csv as pcsv
    with pfs.open_input_stream(f"{B}/{RAW}obs.csv") as f:
        t = pcsv.read_csv(f)
    assert t.num_rows == N
    return f"{t.num_rows:,} rows"


# ------------------------------------------------------------------ DuckDB
con = duckdb.connect()


@step("D1", "duckdb httpfs + CREATE SECRET (STS key/secret/session token, path style)")
def _():
    con.execute("INSTALL httpfs; LOAD httpfs;")
    con.execute(f"""CREATE SECRET eco (TYPE s3, KEY_ID '{C['key']}', SECRET '{C['secret']}',
        SESSION_TOKEN '{C['token']}', ENDPOINT '{EP}', URL_STYLE 'path', USE_SSL false, REGION 'us-east-1')""")
    return "ok"


@step("D2", "duckdb SQL over raw CSV: per-qc aggregates")
def _():
    r = con.execute(f"SELECT qc, count(*) n, round(avg(value),3) a FROM read_csv('s3://{B}/{RAW}obs.csv') "
                    f"GROUP BY qc ORDER BY qc").fetchall()
    exp = obs.groupby("qc")["value"].agg(["count", "mean"]).round(3)
    for qc, n, a in r:
        assert n == exp.loc[qc, "count"] and abs(a - exp.loc[qc, "mean"]) < 1e-3, (qc, n, a)
    return f"{r}"


@step("D3", "duckdb SQL over hive-partitioned parquet glob (partition pruning)")
def _():
    r = con.execute(f"""SELECT day, count(*), round(sum(value),2) FROM read_parquet('s3://{B}/{RUN}arrow/ds/**/*.parquet',
        hive_partitioning=true) WHERE day IN ('2026-01-03','2026-01-04') AND station = 7 GROUP BY day ORDER BY day""").fetchall()
    assert [x[1] for x in r] == [1000, 1000], r
    return f"{r}"


@step("D4", "duckdb COPY query result TO s3 parquet + read back")
def _():
    con.execute(f"""COPY (SELECT station, qc, count(*) n, avg(value) a FROM read_csv('s3://{B}/{RAW}obs.csv')
        GROUP BY ALL) TO 's3://{B}/{RUN}duck/agg.parquet' (FORMAT parquet)""")
    n = con.execute(f"SELECT count(*) FROM 's3://{B}/{RUN}duck/agg.parquet'").fetchone()[0]
    assert n == 600, n
    return f"{n} rows"


@step("D5", "duckdb COPY ... PARTITION_BY (hive) TO s3 + CSV export")
def _():
    con.execute(f"""COPY (SELECT *, strftime(ts, '%Y-%m-%d') d FROM read_csv('s3://{B}/{RAW}obs.csv'))
        TO 's3://{B}/{RUN}duck/part' (FORMAT parquet, PARTITION_BY (d), OVERWRITE_OR_IGNORE)""")
    con.execute(f"COPY (SELECT * FROM 's3://{B}/{RUN}duck/agg.parquet') TO 's3://{B}/{RUN}duck/agg.csv' (HEADER)")
    n = con.execute(f"SELECT count(*) FROM read_parquet('s3://{B}/{RUN}duck/part/**/*.parquet', hive_partitioning=true)").fetchone()[0]
    assert n == N, n
    return f"{n:,} rows across partitions; csv written"


@step("D6", "duckdb write outside the run prefix is denied")
def _():
    return expect_denied(lambda: con.execute(f"COPY (SELECT 1 x) TO 's3://{B}/runs/r2/x.parquet' (FORMAT parquet)"))


# ------------------------------------------------------------------ credential_process bridge
cred_script = os.path.join(WORK, "fa_cred.py")
with open(cred_script, "w") as f:
    f.write(textwrap.dedent(f"""
        import json, boto3
        c = boto3.client("sts", endpoint_url="{URL}", aws_access_key_id="etladmin",
                         aws_secret_access_key="etladmin-secret", region_name="us-east-1").assume_role(
            RoleArn="{ROLE}", RoleSessionName="etl-cp", Policy={json.dumps(json.dumps(POLICY))},
            DurationSeconds=3600)["Credentials"]
        print(json.dumps({{"Version": 1, "AccessKeyId": c["AccessKeyId"], "SecretAccessKey": c["SecretAccessKey"],
                          "SessionToken": c["SessionToken"], "Expiration": c["Expiration"].isoformat()}}))
    """))
cfg = os.path.join(WORK, "aws_config")
with open(cfg, "w") as f:
    f.write(f"[profile fa]\nregion = us-east-1\ncredential_process = {sys.executable} {cred_script}\n"
            f"endpoint_url = {URL}\n")
env = dict(os.environ, AWS_CONFIG_FILE=cfg, AWS_SHARED_CREDENTIALS_FILE=os.devnull, AWS_PROFILE="fa")
for k in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"):
    env.pop(k, None)


def child(code):
    r = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True, timeout=300)
    if r.returncode != 0:
        raise RuntimeError((r.stderr or r.stdout).strip().splitlines()[-1][:300])
    return r.stdout.strip()


@step("K1", "credential_process: s3fs with profile only (no keys in code)")
def _():
    return child(f"""import s3fs; fs = s3fs.S3FileSystem(profile='fa', client_kwargs={{'endpoint_url': '{URL}'}})
print(len(fs.ls('{B}/{RAW}nc')), 'files listed'); fs.pipe('{B}/{RUN}cp/s3fs.txt', b'x')""")


@step("K2", "credential_process: xarray open_dataset with storage_options={'profile': 'fa'}")
def _():
    return child(f"""import xarray as xr
ds = xr.open_dataset('s3://{B}/{RAW}nc/20260103.nc', engine='h5netcdf', chunks={{}},
    storage_options={{'profile': 'fa', 'client_kwargs': {{'endpoint_url': '{URL}'}}}})
print('temp mean', round(float(ds['temp'].isel(time=0).mean()), 4))""")


@step("K3", "credential_process: pyarrow S3FileSystem default credential chain")
def _():
    return child(f"""import pyarrow.fs as f, pyarrow.parquet as pq
fs = f.S3FileSystem(endpoint_override='{EP}', scheme='http', region='us-east-1')
print(pq.read_table('{B}/{RUN}arrow/big.parquet', filesystem=fs).num_rows, 'rows')""")


@step("K4", "credential_process: duckdb secret PROVIDER credential_chain (CHAIN process)")
def _():
    return child(f"""import duckdb; c = duckdb.connect(); c.execute('INSTALL httpfs; LOAD httpfs; INSTALL aws; LOAD aws;')
c.execute(\"CREATE SECRET (TYPE s3, PROVIDER credential_chain, CHAIN 'process', PROFILE 'fa', ENDPOINT '{EP}', URL_STYLE 'path', USE_SSL false, REGION 'us-east-1')\")
print(c.execute(\"SELECT count(*) FROM read_csv('s3://{B}/{RAW}obs.csv')\").fetchone()[0], 'rows')""")


json.dump(RESULTS, open(os.path.join(HERE, "..", "tmp", f"eco-{TARGET}.json"), "w"), indent=1, default=str)
cnt = collections.Counter(r["status"] for r in RESULTS)
print(f"\n== {TARGET}: PASS {cnt['PASS']}  FAIL {cnt['FAIL']}", flush=True)
