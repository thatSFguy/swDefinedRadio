"""Build one compact basemap file from Natural Earth vector data.

Natural Earth is public domain: no permission, no attribution, no usage
policy to fall foul of. The output keeps only what a map under aircraft
icons needs — coastlines, borders, state lines, big lakes and a few
cities — with coordinates rounded to 2 decimal places (about 1 km,
which is a fraction of a pixel at the zoom levels this map is used at).
"""
import json, sys, os

NE = sys.argv[1]
OUT = sys.argv[2]
DP = 2

def rings(path, kinds=("LineString", "MultiLineString", "Polygon", "MultiPolygon"), keep=None):
    with open(os.path.join(NE, path)) as f:
        gj = json.load(f)
    out = []
    for feat in gj["features"]:
        if keep and not keep(feat.get("properties", {})):
            continue
        g = feat.get("geometry")
        if not g or g["type"] not in kinds:
            continue
        t, c = g["type"], g["coordinates"]
        if t == "LineString":
            parts = [c]
        elif t == "MultiLineString":
            parts = c
        elif t == "Polygon":
            parts = [c[0]]                      # outer ring only
        else:
            parts = [poly[0] for poly in c]
        for part in parts:
            line = []
            for lon, lat in part:
                # Leaflet takes lat,lon; storing it that way keeps the
                # page from having to flip every point.
                p = [round(lat, DP), round(lon, DP)]
                if not line or line[-1] != p:
                    line.append(p)
            if len(line) > 1:
                out.append(line)
    return out

def airports(path):
    """Airports, which are what an aircraft map is mostly about: traffic
    is going to or coming from one of these. Natural Earth's set is
    already curated down to the ones worth drawing."""
    with open(os.path.join(NE, path)) as f:
        gj = json.load(f)
    out = []
    for feat in gj["features"]:
        p = feat["properties"]
        code = p.get("abbrev") or p.get("iata_code") or p.get("gps_code")
        if not code:
            continue
        lon, lat = feat["geometry"]["coordinates"]
        a = {
            # Points, so the extra precision is nearly free: 4 places is
            # about 11 m, which puts the symbol on the right runway.
            "p": [round(lat, 4), round(lon, 4)],
            "c": code,
            "n": p.get("name", code),
            "r": p.get("scalerank", 9),
        }
        if "military" in (p.get("type") or ""):
            a["m"] = 1
        out.append(a)
    out.sort(key=lambda x: x["r"])
    return out

def places(path):
    with open(os.path.join(NE, path)) as f:
        gj = json.load(f)
    out = []
    for feat in gj["features"]:
        p = feat["properties"]
        rank = p.get("scalerank", 20)
        pop = p.get("pop_max") or 0
        # Enough cities to orient by, not enough to clutter: the ones
        # a small-scale map would label, plus anywhere big.
        if rank > 6 and pop < 500000:
            continue
        lon, lat = feat["geometry"]["coordinates"]
        out.append({"n": p.get("name", "?"), "p": [round(lat, DP), round(lon, DP)], "r": rank})
    out.sort(key=lambda x: x["r"])
    return out

data = {
    # Land as polygons for the fill, and the coastline again as lines
    # for the outline. They are the same shapes, but a filled polygon
    # has to be drawn without a stroke: the canvas renderer clips
    # polygons to the viewport and strokes the clip edge, which draws a
    # box across the map. Clipped polylines have no such edge.
    "land":    rings("ne_110m_land.geojson"),
    "coast":   rings("ne_110m_coastline.geojson"),
    "borders": rings("ne_110m_admin_0_boundary_lines_land.geojson"),
    "states":  rings("ne_50m_admin_1_states_provinces_lines.geojson"),
    "lakes":   rings("ne_110m_lakes.geojson", keep=lambda p: (p.get("scalerank") or 0) <= 3),
    "places":  places("ne_50m_populated_places_simple.geojson"),
    "airports": airports("ne_10m_airports.geojson"),
}
with open(OUT, "w") as f:
    json.dump(data, f, separators=(",", ":"))

for k, v in data.items():
    pts = len(v) if k in ("places", "airports") else sum(len(r) for r in v)
    print(f"{k:8} {len(v):5} features {pts:7} points")
print("total", os.path.getsize(OUT) // 1024, "KB")
